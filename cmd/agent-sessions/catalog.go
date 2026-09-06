package main

import (
	"context"
	"errors"
	"io"
	"reflect"

	"github.com/antst/sessionbus/internal/clihelp"
	"github.com/antst/sessionbus/internal/productcatalog"
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
