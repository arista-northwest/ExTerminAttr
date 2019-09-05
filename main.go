// Copyright (c) 2019 Arista Networks, Inc.
// Use of this source code is governed by the Apache License 2.0
// that can be found in the COPYING file.

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"os"
	"path"
	"strings"
	"time"

	"github.com/aristanetworks/glog"
	"github.com/aristanetworks/goarista/gnmi"
	pb "github.com/openconfig/gnmi/proto/gnmi"
)

var help = `Usage of exterminattr:
exterminattr -addr ADDRESS:PORT -forward_url URL:PORT [options...] PATH+
sudo /sbin/ip netns exec ns-management exterminattr ...
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
	//nsName := flag.String("ns_name", "default", "")

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

	if cfg.Addr == "" {
		usageAndExit("error: address not specified")
	}
	args := flag.Args()

	ctx := gnmi.NewContext(context.Background(), cfg)
	client, err := gnmi.Dial(cfg)
	if err != nil {
		glog.Fatal(err)
	}

	respChan := make(chan *pb.SubscribeResponse)
	errChan := make(chan error)
	defer close(errChan)

	if *pathsFile != "" {
		paths, err = loadPaths(*pathsFile)
		if err != nil {
			glog.Fatal(err)
		}
	} else {
		paths = args[:]
	}

	subscribeOptions.Paths = gnmi.SplitPaths(paths)

	go gnmi.Subscribe(ctx, client, subscribeOptions, respChan, errChan)

	for {
		select {
		case resp, open := <-respChan:
			if !open {
				return
			}
			if err := forwardSubscribeResponse(resp, *forwardURL); err != nil {
				glog.Fatal(err)
			}
		case err := <-errChan:
			glog.Fatal(err)
		}
	}
}

func loadPaths(pathsFile string) ([]string, error) {
	file, err := os.Open(pathsFile)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	b, err := ioutil.ReadAll(file)
	trimmed := strings.TrimSpace(string(b))
	return strings.Split(trimmed, " "), nil
}

func forwardSubscribeResponse(response *pb.SubscribeResponse, forwardURL string) error {
	type Update struct {
		Timestamp string      `json:"timestamp"`
		Operation string      `json:"operation"`
		Path      string      `json:"path"`
		Value     interface{} `json:"value"`
	}

	// if err := gnmi.LogSubscribeResponse(response); err != nil {
	// 	glog.Fatal(err)
	// }

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
			updates = append(updates, Update{
				Timestamp: timetstamp.Format(time.RFC3339Nano),
				Operation: "UPDATE",
				Path:      path.Join(prefix, gnmi.StrPath(update.Path)),
				Value:     gnmi.StrUpdateVal(update),
			})
		}

		for _, del := range resp.Update.Delete {
			updates = append(updates, Update{
				Timestamp: timetstamp.Format(time.RFC3339Nano),
				Operation: "DELETE",
				Path:      path.Join(prefix, gnmi.StrPath(del)),
				Value:     nil,
			})
		}

		data, err := json.Marshal(updates)
		if err != nil {
			glog.Fatal(err)
		}

		err = forward(forwardURL, data)
		if err != nil {
			log.Fatal(err)
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
			//TLSClientConfig: ..., <- if you need SSL/TLS.
			Dial: dial,
		},
		Timeout: 30 * time.Second, // This is the request timeout
	}

	_, err := client.Post(url, "application/json", bytes.NewBuffer(data))
	return err
}
