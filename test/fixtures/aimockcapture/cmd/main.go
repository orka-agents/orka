/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/orka-agents/orka/test/fixtures/aimockcapture"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string) error {
	flags := flag.NewFlagSet("aimockcapture", flag.ContinueOnError)
	// Flag parsing can otherwise echo untrusted values, including upstream userinfo.
	flags.SetOutput(io.Discard)
	listen := flags.String("listen", ":4010", "listen address")
	upstream := flags.String("upstream", "http://127.0.0.1:4011", "aimock upstream URL")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		return errors.New("invalid command flags")
	}
	target, err := url.Parse(*upstream)
	if err != nil {
		return errors.New("invalid upstream URL")
	}
	handler, err := aimockcapture.New(target)
	if err != nil {
		return err
	}
	server := &http.Server{
		Addr: *listen, Handler: handler,
		ReadHeaderTimeout: 5 * time.Second,
		ErrorLog:          log.New(io.Discard, "", 0),
	}
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return errors.New("capture server failed")
	}
	return nil
}
