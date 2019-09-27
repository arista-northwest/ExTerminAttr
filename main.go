// Copyright (c) 2019 Arista Networks, Inc.
// Use of this source code is governed by the Apache License 2.0
// that can be found in the COPYING file.

package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"os"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/aristanetworks/goarista/gnmi"
	"github.com/jpillora/backoff"
	pb "github.com/openconfig/gnmi/proto/gnmi"
	log "github.com/sirupsen/logrus"
)

var help = `Usage of ExTerminAttr:
ExTerminAttr -forward_url URL:PORT [origin=ORIGIN] PATH+
sudo /sbin/ip netns exec ns-management ExTerminAttr -forward_url URL:PORT [origin=ORIGIN] PATH+
`

func usageAndExit(s string) {
	flag.Usage()
	if s != "" {
		fmt.Fprintln(os.Stderr, s)
	}
	os.Exit(1)
}

func main() {
	cfg := &gnmi.Config{}

	subscribeOptions := &gnmi.SubscribeOptions{}

	level := flag.Int("log_level", 4, "Set logging level [0-5]. Default is 4 (INFO)")

	flag.StringVar(&cfg.Addr, "addr", "localhost:6042", "Address of gNMI gRPC server with optional VRF name")
	forwardURL := flag.String("forward_url", "http://localhost:8080", "")
	pathsFile := flag.String("paths_file", "", "path to file")
	flag.StringVar(&cfg.Password, "password", "", "Password to authenticate with")
	flag.StringVar(&cfg.Username, "username", "", "Username to authenticate with")
	flag.StringVar(&subscribeOptions.Origin, "origin", "", "")
	flag.StringVar(&subscribeOptions.Prefix, "prefix", "", "Subscribe prefix path")
	flag.BoolVar(&subscribeOptions.UpdatesOnly, "updates_only", false,
		"Subscribe to updates only (false | true)")
	flag.StringVar(&subscribeOptions.Mode, "mode", "stream",
		"Subscribe mode (stream | once | poll)")
	flag.StringVar(&subscribeOptions.StreamMode, "stream_mode", "target_defined",
		"Subscribe stream mode, only applies for stream subscriptions "+
			"(target_defined | on_change | sample)")
	sampleIntervalStr := flag.String("sample_interval", "0", "Subscribe sample interval, "+
		"only applies for sample subscriptions (400ms, 2.5s, 1m, etc.)")
	heartbeatIntervalStr := flag.String("heartbeat_interval", "0", "Subscribe heartbeat "+
		"interval, only applies for on-change subscriptions (400ms, 2.5s, 1m, etc.)")

	var paths []string
	var sampleInterval, heartbeatInterval time.Duration
	var err error

	if sampleInterval, err = time.ParseDuration(*sampleIntervalStr); err != nil {
		usageAndExit(fmt.Sprintf("error: sample interval (%s) invalid", *sampleIntervalStr))
	}
	subscribeOptions.SampleInterval = uint64(sampleInterval)
	if heartbeatInterval, err = time.ParseDuration(*heartbeatIntervalStr); err != nil {
		usageAndExit(fmt.Sprintf("error: heartbeat interval (%s) invalid", *heartbeatIntervalStr))
	}
	subscribeOptions.HeartbeatInterval = uint64(heartbeatInterval)

	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, help)
		flag.PrintDefaults()
	}

	flag.Parse()

	log.SetLevel(log.AllLevels[*level])

	if cfg.Addr == "" {
		usageAndExit("error: address not specified")
	}
	args := flag.Args()
	var origin string

	if len(args) > 0 {
		origin = parseOrigin(args[0])
		if origin != "" {
			// offet args if origin is set
			args = args[1:]
		}
	}

	if *pathsFile != "" {
		paths, err = loadPaths(*pathsFile)
		if err != nil {
			log.Fatal(err)
		}
	} else {
		paths = args[:]
	}

	log.Infof("Loaded paths: %s", paths)

	subscribeOptions.Paths = gnmi.SplitPaths(paths)

	log.Infof("Setting orgin=%s", origin)
	subscribeOptions.Origin = origin

	b := &backoff.Backoff{
		//These are the defaults
		Min:    100 * time.Millisecond,
		Max:    10 * time.Second,
		Factor: 2,
		Jitter: false,
	}

	log.Infof("Backoff set to: min:%dms, max:%dms, factor:%.f, jitter:%t",
		b.Min/1000000, b.Max/1000000, b.Factor, b.Jitter)

	for {
		log.Info("(Re)starting subscribe and forward")
		err = subscribeAndForward(cfg, subscribeOptions, *forwardURL)
		if err != nil {
			d := b.Duration()
			log.Warnf(fmt.Sprintf("%s, retrying in %s\n", err, d))
			time.Sleep(d)
			continue
		}
	}
}

func forwardSubscribeResponse(response *pb.SubscribeResponse, forwardURL string) error {
	type Update struct {
		Timestamp string      `json:"timestamp"`
		Operation string      `json:"operation"`
		Path      string      `json:"path"`
		Value     interface{} `json:"value"`
	}

	switch resp := response.Response.(type) {

	case *pb.SubscribeResponse_Error:
		return errors.New(resp.Error.Message)

	case *pb.SubscribeResponse_SyncResponse:
		if !resp.SyncResponse {
			return errors.New("initial sync failed")
		}

	case *pb.SubscribeResponse_Update:
		timetstamp := time.Unix(0, resp.Update.Timestamp).UTC()
		prefix := gnmi.StrPath(resp.Update.Prefix)

		var updates []Update
		for _, update := range resp.Update.Update {
			value, err := gnmi.ExtractValue(update)
			if err != nil {
				log.Error("Failed to extract value:", err)
				return nil
			}
			p := path.Join(prefix, gnmi.StrPath(update.Path))
			//log.Debugf("Got [UPDATE]: %s=%s", p, value)
			updates = append(updates, Update{
				Timestamp: timetstamp.Format(time.RFC3339Nano),
				Operation: "UPDATE",
				Path:      p,
				Value:     value,
			})
		}

		for _, del := range resp.Update.Delete {
			p := path.Join(prefix, gnmi.StrPath(del))
			//log.Debugf("Got [DELETE]: %s", p)
			updates = append(updates, Update{
				Timestamp: timetstamp.Format(time.RFC3339Nano),
				Operation: "DELETE",
				Path:      p,
				Value:     nil,
			})
		}

		data, err := json.Marshal(updates)
		if err != nil {
			return err
		}

		log.Debugf("Forwarding response %s", data)
		err = forward(forwardURL, data)
		if err != nil {
			return err
		}
	}

	return nil
}

func forward(url string, data []byte) error {
	dial := func(network, address string) (net.Conn, error) {
		conn, err := (&net.Dialer{
			Timeout:   30 * time.Second, // This is the connection timeout
			KeepAlive: 30 * time.Second,
		}).Dial(network, address)
		return conn, err
	}

	client := &http.Client{
		Transport: &http.Transport{
			// Don't try to validate certificates
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
			Dial:            dial,
		},
		Timeout: 30 * time.Second, // This is the request timeout
	}

	//req, err := client.Post(url, "application/json", bytes.NewBuffer(data))
	req, err := http.NewRequest("POST", url, bytes.NewBuffer(data))

	defer func() {
		req.Header.Set("Connection", "close")
		req.Close = true
	}()

	resp, err := client.Do(req)

	defer func() {
		if resp != nil {
			resp.Body.Close()
		}
	}()

	if err != nil {
		return err
	}

	return nil
}

func loadPaths(pathsFile string) ([]string, error) {
	file, err := os.Open(pathsFile)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	data, err := ioutil.ReadAll(file)
	trimmed := strings.TrimSpace(string(data))

	s := regexp.MustCompile("[[:space:]]+").Split(trimmed, -1)

	return s, nil
}

func parseOrigin(s string) string {

	if strings.HasPrefix(s, "origin=") {
		return strings.TrimPrefix(s, "origin=")
	}
	return ""
}

func subscribeAndForward(cfg *gnmi.Config, subscribeOptions *gnmi.SubscribeOptions, forwardURL string) error {

	ctx := gnmi.NewContext(context.Background(), cfg)
	client, err := gnmi.Dial(cfg)
	if err != nil {
		return err
	}

	respChan := make(chan *pb.SubscribeResponse, 10)
	errChan := make(chan error, 10)
	defer close(errChan)

	log.Infof("Subscribing to paths: %s", subscribeOptions.Paths)

	go gnmi.Subscribe(ctx, client, subscribeOptions, respChan, errChan)

	for {
		select {
		case resp, open := <-respChan:
			if !open {
				return err
			}

			if err := forwardSubscribeResponse(resp, forwardURL); err != nil {
				return err
			}
		case err := <-errChan:
			return err
		}
	}
}
