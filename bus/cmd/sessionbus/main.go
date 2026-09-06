package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/antst/sessionbus/bus/internal/daemon"
	"github.com/antst/sessionbus/bus/internal/stateroot"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(arguments []string) error {
	configuration, err := parse(arguments)
	if err != nil {
		return err
	}
	service, err := daemon.Start(configuration)
	if err != nil {
		return err
	}
	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(interrupt)
	select {
	case <-interrupt:
	case <-service.Done():
	}
	return service.Close()
}

func parse(arguments []string) (daemon.Config, error) {
	root, err := stateroot.Resolve()
	if err != nil {
		return daemon.Config{}, err
	}
	socket, err := stateroot.SessionSocket()
	if err != nil {
		return daemon.Config{}, err
	}
	set := flag.NewFlagSet("sessionbus", flag.ContinueOnError)
	configuration := daemon.Config{}
	products := ""
	set.StringVar(&configuration.SocketPath, "socket", socket, "unix socket path")
	set.StringVar(&configuration.TablePath, "table", filepath.Join(root, "sessions.json"), "durable session table")
	set.StringVar(&configuration.Host, "host", "", "local host name")
	set.StringVar(&products, "products", "", "comma-separated advertised products")
	if err := set.Parse(arguments); err != nil {
		return daemon.Config{}, err
	}
	if set.NArg() != 0 {
		return daemon.Config{}, fmt.Errorf("unexpected argument %q", set.Arg(0))
	}
	if products != "" {
		configuration.Products = strings.Split(products, ",")
	}
	return configuration, nil
}
