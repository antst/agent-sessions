package main

import (
	"context"
	"errors"
	"io"
	"reflect"

	"github.com/antst/agent-sessions/internal/clihelp"
	"github.com/antst/agent-sessions/internal/productcatalog"
)

func runCatalog(_ context.Context, invocation clihelp.Invocation, output io.Writer) error {
	if !reflect.DeepEqual(invocation.Arguments, []string{"--json"}) {
		return errors.New("agent-sessions catalog requires exactly --json")
	}
	body, err := productcatalog.ProjectionJSON(productcatalog.All())
	if err != nil {
		return err
	}
	_, err = output.Write(body)
	return err
}
