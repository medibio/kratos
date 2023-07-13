// Copyright © 2023 Ory Corp
// SPDX-License-Identifier: Apache-2.0

package flow

import (
	"context"
	_ "embed"
	"net/http"
	"strings"

	"github.com/ory/kratos/driver/config"
	"github.com/ory/kratos/identity"
	"github.com/ory/kratos/selfservice/strategy"
	"github.com/ory/x/decoderx"

	"github.com/pkg/errors"

	"github.com/ory/herodot"
	"github.com/ory/kratos/x"
	"github.com/ory/nosurf"
)

//go:embed .schema/method.schema.json
var methodSchema []byte

var ErrOriginHeaderNeedsBrowserFlow = herodot.ErrBadRequest.
	WithReasonf(`The HTTP Request Header included the "Origin" key, indicating that this request was made as part of an AJAX request in a Browser. The flow however was initiated as an API request. To prevent potential misuse and mitigate several attack vectors including CSRF, the request has been blocked. Please consult the documentation.`)

var ErrCookieHeaderNeedsBrowserFlow = herodot.ErrBadRequest.
	WithReasonf(`The HTTP Request Header included the "Cookie" key, indicating that this request was made by a Browser. The flow however was initiated as an API request. To prevent potential misuse and mitigate several attack vectors including CSRF, the request has been blocked. Please consult the documentation.`)

func EnsureCSRF(reg interface {
	config.Provider
},
	r *http.Request,
	flowType Type,
	disableAPIFlowEnforcement bool,
	generator func(r *http.Request) string,
	actual string,
) error {
	switch flowType {
	case TypeAPI:
		if disableAPIFlowEnforcement {
			return nil
		}

		// API Based flows to not require anti-CSRF tokens because we can not leverage a session, making this
		// endpoint pointless.

		// Let's ensure that no-one mistakenly makes an AJAX request using the API flow.
		if r.Header.Get("Origin") != "" {
			return errors.WithStack(ErrOriginHeaderNeedsBrowserFlow)
		}

		// Workaround for Cloudflare setting cookies that we can't control.
		var hasCookie bool
		for _, c := range r.Cookies() {
			if !strings.HasPrefix(c.Name, "__cf") {
				hasCookie = true
				break
			}
		}

		if hasCookie {
			return errors.WithStack(ErrCookieHeaderNeedsBrowserFlow)
		}

		return nil
	default:
		if !nosurf.VerifyToken(generator(r), actual) {
			return errors.WithStack(x.CSRFErrorReason(r, reg))
		}
	}

	return nil
}

var dec = decoderx.NewHTTP()

func MethodEnabledAndAllowedFromRequest(r *http.Request, flow FlowName, expected string, d interface {
	config.Provider
},
) error {
	var method struct {
		Method string `json:"method" form:"method"`
	}

	compiler, err := decoderx.HTTPRawJSONSchemaCompiler(methodSchema)
	if err != nil {
		return errors.WithStack(err)
	}

	if err := dec.Decode(r, &method, compiler,
		decoderx.HTTPKeepRequestBody(true),
		decoderx.HTTPDecoderAllowedMethods("POST", "PUT", "PATCH", "GET"),
		decoderx.HTTPDecoderSetValidatePayloads(false),
		decoderx.HTTPDecoderJSONFollowsFormFormat()); err != nil {
		return errors.WithStack(err)
	}

	return MethodEnabledAndAllowed(r.Context(), flow, expected, method.Method, d)
}

// TODO: to disable specific flows we need to pass down the flow somehow to this method
// we could do this by adding an additional parameter, but not all methods have access to the flow
// this adds a lot of refactoring work, so we should think about a better way to do this
func MethodEnabledAndAllowed(ctx context.Context, flowName FlowName, expected, actual string, d interface {
	config.Provider
},
) error {
	if actual != expected {
		return errors.WithStack(ErrStrategyNotResponsible)
	}

	var ok bool

	if strings.EqualFold(actual, identity.CredentialsTypeCodeAuth.String()) {
		switch flowName {
		case RegistrationFlow:
			ok = d.Config().SelfServiceCodeStrategy(ctx).RegistrationEnabled
		case LoginFlow:
			ok = d.Config().SelfServiceCodeStrategy(ctx).LoginEnabled
		default:
			ok = d.Config().SelfServiceCodeStrategy(ctx).Enabled
		}
	} else {
		ok = d.Config().SelfServiceStrategy(ctx, expected).Enabled
	}

	if !ok {
		return herodot.ErrNotFound.WithReason(strategy.EndpointDisabledMessage)
	}

	return nil
}
