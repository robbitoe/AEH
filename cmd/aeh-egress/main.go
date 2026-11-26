// Command aeh-egress is the network side of an aeh agent sandbox (stage 6, S3). It is
// built into the agent image; aeh runs it, people do not.
//
//	aeh-egress proxy   --socket <path> --policy <file>
//	aeh-egress forward --socket <path> --listen <addr>
//	aeh-egress launch  --socket <path> --listen <addr> [--wait <duration>] -- <program> [args...]
//
// proxy serves CONNECT on a Unix socket, only to the policy's host:port pairs; it runs in
// its own container, the only one of a launch with a network. forward relays a TCP port
// to that socket. launch is an agent container's entrypoint: it starts the forwarder,
// waits for the proxy, then runs the agent's program.
package main

import (
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/robbitoe/aeh/internal/egress"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	switch os.Args[1] {
	case "proxy":
		fs := flag.NewFlagSet("proxy", flag.ExitOnError)
		socket := fs.String("socket", "", "Unix socket to serve")
		policy := fs.String("policy", "", "policy file (JSON)")
		_ = fs.Parse(os.Args[2:])
		if *socket == "" || *policy == "" || fs.NArg() != 0 {
			usage()
		}
		if err := proxy(*socket, *policy); err != nil {
			fmt.Fprintln(os.Stderr, "aeh-egress proxy:", err)
			os.Exit(1)
		}
	case "forward":
		fs := flag.NewFlagSet("forward", flag.ExitOnError)
		socket := fs.String("socket", "", "proxy socket")
		listen := fs.String("listen", "", "TCP address to listen on")
		_ = fs.Parse(os.Args[2:])
		if *socket == "" || *listen == "" || fs.NArg() != 0 {
			usage()
		}
		l, err := net.Listen("tcp", *listen)
		if err == nil {
			err = egress.Forward(l, *socket)
		}
		fmt.Fprintln(os.Stderr, "aeh-egress forward:", err)
		os.Exit(1)
	case "launch":
		fs := flag.NewFlagSet("launch", flag.ExitOnError)
		socket := fs.String("socket", "", "proxy socket")
		listen := fs.String("listen", "", "TCP address of the forwarder")
		wait := fs.Duration("wait", 30*time.Second, "how long to wait for the proxy")
		_ = fs.Parse(os.Args[2:])
		if *socket == "" || *listen == "" || fs.NArg() == 0 {
			usage()
		}
		os.Exit(egress.Launch(*socket, *listen, *wait, fs.Args(), os.Stderr))
	default:
		usage()
	}
}

func proxy(socket, policyFile string) error {
	f, err := os.Open(policyFile)
	if err != nil {
		return err
	}
	data, err := io.ReadAll(io.LimitReader(f, 1<<20))
	f.Close()
	if err != nil {
		return err
	}
	p, err := egress.ParsePolicy(data)
	if err != nil {
		return err
	}
	if err := os.Remove(socket); err != nil && !os.IsNotExist(err) {
		return err
	}
	l, err := net.Listen("unix", socket)
	if err != nil {
		return err
	}
	// SIGTERM (docker stop) closes every tunnel and lets each write its final log line.
	px := &egress.Proxy{Policy: p, Log: os.Stdout}
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, os.Interrupt)
	served := make(chan error, 1)
	go func() { served <- px.Serve(l) }()
	select {
	case <-sigs:
		px.Shutdown(5 * time.Second)
		return nil
	case err := <-served:
		return err
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: aeh-egress proxy --socket <path> --policy <file>\n"+
		"       aeh-egress forward --socket <path> --listen <addr>\n"+
		"       aeh-egress launch --socket <path> --listen <addr> [--wait <duration>] -- <program> [args...]")
	os.Exit(2)
}
