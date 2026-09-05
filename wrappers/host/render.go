package host

import (
	sessionkit "github.com/antst/agent-sessions/bus/sdk/go"
	"github.com/antst/agent-sessions/internal/productruntime"
	"github.com/antst/agent-sessions/internal/sessiontools"
)

func render(request sessionkit.DeliveryRequest) (string, error) {
	return sessiontools.RenderNativeMessage(productruntime.NativeMessage{
		ID: request.MessageID, Body: request.Body,
		From: productruntime.NativeMessageSource{
			UUID: request.From.SessionID, Name: request.From.Name,
			Product: request.From.Product, Groups: request.From.Groups,
		},
	})
}
