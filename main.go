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
	"log"
	"net/http"
	"os"
	"path"
	"time"

	"github.com/aristanetworks/glog"
	"github.com/aristanetworks/goarista/gnmi"
	pb "github.com/openconfig/gnmi/proto/gnmi"
)

// TODO: Make this more clear
var help = `Usage of gnmi:
gnmi -addr [<VRF-NAME>/]ADDRESS:PORT -forwardurl URL:PORT [options...] PATH+
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
	forward_url := flag.String("forward_url", "http://localhost:8080", "")

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
	subscribeOptions.Paths = gnmi.SplitPaths(args[:])
	go gnmi.Subscribe(ctx, client, subscribeOptions, respChan, errChan)
	for {
		select {
		case resp, open := <-respChan:
			if !open {
				return
			}
			if err := ForwardSubscribeResponse(resp, forward_url); err != nil {
				glog.Fatal(err)
			}
		case err := <-errChan:
			glog.Fatal(err)
		}
	}
}

func ForwardSubscribeResponse(response *pb.SubscribeResponse, forward_url *string) error {
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
		t := time.Unix(0, resp.Update.Timestamp).UTC()
		message := Update{
			Timestamp: t.Format(time.RFC3339Nano),
		}
		prefix := gnmi.StrPath(resp.Update.Prefix)
		for _, update := range resp.Update.Update {
			message.Operation = "UPDATE"
			message.Path = path.Join(prefix, gnmi.StrPath(update.Path))
			message.Value = gnmi.StrUpdateVal(update)
		}

		for _, del := range resp.Update.Delete {
			message.Operation = "DELETE"
			message.Path = path.Join(prefix, gnmi.StrPath(del))
			message.Value = nil
		}

		data, err := json.Marshal(message)
		if err != nil {
			glog.Fatal(err)
		}

		// TODO: Forward here...
		_, err = http.Post(*forward_url, "application/json", bytes.NewBuffer(data))
		if err != nil {
			log.Fatal(err)
		}
	}

	return nil
}
