// Copyright © 2023 Ory Corp
// SPDX-License-Identifier: Apache-2.0

package hook

import (
	"net/http"

	"github.com/pkg/errors"

	"github.com/ory/kratos/driver/config"
	"github.com/ory/kratos/identity"
	"github.com/ory/kratos/selfservice/flow/login"
	"github.com/ory/kratos/selfservice/flow/registration"
	"github.com/ory/kratos/selfservice/flow/verification"
	"github.com/ory/kratos/selfservice/strategy/code"
	"github.com/ory/kratos/session"
	"github.com/ory/kratos/ui/node"
	"github.com/ory/kratos/x"
)

type (
	codeAddressDependencies interface {
		config.Provider
		x.CSRFTokenGeneratorProvider
		x.CSRFProvider
		verification.StrategyProvider
		verification.FlowPersistenceProvider
		code.RegistrationCodePersistenceProvider
		code.LoginCodePersistenceProvider
		identity.PrivilegedPoolProvider
		x.WriterProvider
	}
	CodeAddressVerifier struct {
		r codeAddressDependencies
	}
)

var (
	_ registration.PostHookPostPersistExecutor = new(CodeAddressVerifier)
	_ login.PostHookExecutor                   = new(CodeAddressVerifier)
)

func NewCodeAddressVerifier(r codeAddressDependencies) *CodeAddressVerifier {
	return &CodeAddressVerifier{r: r}
}

func (cv *CodeAddressVerifier) ExecuteLoginPostHook(_ http.ResponseWriter, r *http.Request, _ node.UiNodeGroup, f *login.Flow, s *session.Session) error {
	if f.Active != identity.CredentialsTypeCodeAuth {
		return nil
	}

	loginCode, err := cv.r.LoginCodePersister().GetUsedLoginCode(r.Context(), f.GetID())
	if err != nil {
		return errors.WithStack(err)
	}

	for idx := range s.Identity.VerifiableAddresses {
		va := s.Identity.VerifiableAddresses[idx]
		if !va.Verified && loginCode.Address == va.Value {
			va.Verified = true
			va.Status = identity.VerifiableAddressStatusCompleted
			if err := cv.r.PrivilegedIdentityPool().UpdateVerifiableAddress(r.Context(), &va); err != nil {
				return errors.WithStack(err)
			}
			break
		}
	}
	return nil
}

func (cv *CodeAddressVerifier) ExecutePostRegistrationPostPersistHook(w http.ResponseWriter, r *http.Request, a *registration.Flow, s *session.Session) error {
	if a.Active != identity.CredentialsTypeCodeAuth {
		return nil
	}

	recoveryCode, err := cv.r.RegistrationCodePersister().GetUsedRegistrationCode(r.Context(), a.GetID())
	if err != nil {
		return errors.WithStack(err)
	}

	for idx := range s.Identity.VerifiableAddresses {
		va := s.Identity.VerifiableAddresses[idx]
		if !va.Verified && recoveryCode.Address == va.Value {
			va.Verified = true
			va.Status = identity.VerifiableAddressStatusCompleted
			if err := cv.r.PrivilegedIdentityPool().UpdateVerifiableAddress(r.Context(), &va); err != nil {
				return errors.WithStack(err)
			}
			break
		}
	}

	return nil
}
