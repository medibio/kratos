// Copyright © 2023 Ory Corp
// SPDX-License-Identifier: Apache-2.0

package code

import (
	"net/http"
	"net/url"
	"time"

	"github.com/gofrs/uuid"
	"github.com/pkg/errors"

	"github.com/ory/x/decoderx"
	"github.com/ory/x/sqlxx"
	"github.com/ory/x/urlx"

	"github.com/ory/kratos/identity"
	"github.com/ory/kratos/schema"
	"github.com/ory/kratos/selfservice/flow"
	"github.com/ory/kratos/selfservice/flow/recovery"
	"github.com/ory/kratos/session"
	"github.com/ory/kratos/text"
	"github.com/ory/kratos/ui/container"
	"github.com/ory/kratos/ui/node"
	"github.com/ory/kratos/x"
)

func (s *Strategy) RecoveryStrategyID() string {
	return string(recovery.RecoveryStrategyCode)
}

func (s *Strategy) PopulateRecoveryMethod(r *http.Request, f *recovery.Flow) error {
	f.UI.SetCSRF(s.deps.GenerateCSRFToken(r))
	f.UI.GetNodes().Upsert(
		node.NewInputField("email", nil, node.CodeGroup, node.InputAttributeTypeEmail, node.WithRequiredInputAttribute).
			WithMetaLabel(text.NewInfoNodeInputEmail()),
	)
	f.UI.
		GetNodes().
		Append(node.NewInputField("method", s.RecoveryStrategyID(), node.CodeGroup, node.InputAttributeTypeSubmit).
			WithMetaLabel(text.NewInfoNodeLabelSubmit()))

	return nil
}

// Update Recovery Flow with Code Method
//
// swagger:model updateRecoveryFlowWithCodeMethod
//
//nolint:deadcode,unused
//lint:ignore U1000 Used to generate Swagger and OpenAPI definitions
type updateRecoveryFlowWithCodeMethod struct {
	// The email address of the account to recover
	//
	// If the email belongs to a valid account, a recovery email will be sent.
	//
	// If you want to notify the email address if the account does not exist, see
	// the [notify_unknown_recipients flag](https://www.ory.sh/docs/kratos/self-service/flows/account-recovery-password-reset#attempted-recovery-notifications)
	//
	// If a code was already sent, including this field in the payload will invalidate the sent code and re-send a new code.
	//
	// format: email
	// required: false
	Email string `json:"email" form:"email"`

	// Code from the recovery email
	//
	// If you want to submit a code, use this field, but make sure to _not_ include the email field, as well.
	//
	// required: false
	Code string `json:"code" form:"code"`

	// Sending the anti-csrf token is only required for browser login flows.
	CSRFToken string `form:"csrf_token" json:"csrf_token"`

	// Method is the method that should be used for this recovery flow
	//
	// Allowed values are `link` and `code`.
	//
	// required: true
	Method recovery.RecoveryMethod `json:"method"`
}

func (s Strategy) isCodeFlow(f *recovery.Flow) bool {
	value, err := f.Active.Value()
	if err != nil {
		return false
	}
	return value == s.RecoveryNodeGroup().String()
}

func (s *Strategy) Recover(w http.ResponseWriter, r *http.Request, f *recovery.Flow) (err error) {
	if !s.isCodeFlow(f) {
		return errors.WithStack(flow.ErrStrategyNotResponsible)
	}

	body, err := s.decodeRecovery(r)
	if err != nil {
		return s.HandleRecoveryError(w, r, nil, body, err)
	}
	ctx := r.Context()

	if f.DangerousSkipCSRFCheck {
		s.deps.Logger().
			WithRequest(r).
			Debugf("A recovery flow with `DangerousSkipCSRFCheck` set has been submitted, skipping anti-CSRF measures.")
	} else if err := flow.EnsureCSRF(s.deps, r, f.Type, s.deps.Config().DisableAPIFlowEnforcement(ctx), s.deps.GenerateCSRFToken, body.CSRFToken); err != nil {
		// If a CSRF violation occurs the flow is most likely FUBAR, as the user either lost the CSRF token, or an attack occured.
		// In this case, we just issue a new flow and "abandon" the old flow.
		return s.retryRecoveryFlowWithError(w, r, flow.TypeBrowser, err)
	}

	sID := s.RecoveryStrategyID()

	f.UI.ResetMessages()

	// If the email is present in the submission body, the user needs a new code via resend
	if f.State != recovery.StateChooseMethod && len(body.Email) == 0 {
		if err := flow.MethodEnabledAndAllowed(ctx, sID, sID, s.deps); err != nil {
			return s.HandleRecoveryError(w, r, nil, body, err)
		}
		return s.recoveryUseCode(w, r, body, f)
	}

	if _, err := s.deps.SessionManager().FetchFromRequest(ctx, r); err == nil {
		// User is already logged in
		if x.IsJSONRequest(r) {
			session.RespondWithJSONErrorOnAuthenticated(s.deps.Writer(), recovery.ErrAlreadyLoggedIn)(w, r, nil)
		} else {
			session.RedirectOnAuthenticated(s.deps)(w, r, nil)
		}
		return errors.WithStack(flow.ErrCompletedByStrategy)
	}

	if err := flow.MethodEnabledAndAllowed(ctx, sID, body.Method, s.deps); err != nil {
		return s.HandleRecoveryError(w, r, nil, body, err)
	}

	flow, err := s.deps.RecoveryFlowPersister().GetRecoveryFlow(ctx, x.ParseUUID(body.Flow))
	if err != nil {
		return s.HandleRecoveryError(w, r, flow, body, err)
	}

	if err := flow.Valid(); err != nil {
		return s.HandleRecoveryError(w, r, flow, body, err)
	}

	switch flow.State {
	case recovery.StateChooseMethod:
		fallthrough
	case recovery.StateEmailSent:
		return s.recoveryHandleFormSubmission(w, r, flow, body)
	case recovery.StatePassedChallenge:
		// was already handled, do not allow retry
		return s.retryRecoveryFlowWithMessage(w, r, flow.Type, text.NewErrorValidationRecoveryRetrySuccess())
	default:
		return s.retryRecoveryFlowWithMessage(w, r, flow.Type, text.NewErrorValidationRecoveryStateFailure())
	}
}

func (s *Strategy) recoveryIssueSession(w http.ResponseWriter, r *http.Request, f *recovery.Flow, id *identity.Identity) error {
	ctx := r.Context()

	f.UI.Messages.Clear()
	f.State = recovery.StatePassedChallenge
	f.SetCSRFToken(s.deps.CSRFHandler().RegenerateToken(w, r))
	f.RecoveredIdentityID = uuid.NullUUID{
		UUID:  id.ID,
		Valid: true,
	}
	if err := s.deps.RecoveryFlowPersister().UpdateRecoveryFlow(ctx, f); err != nil {
		return s.retryRecoveryFlowWithError(w, r, f.Type, err)
	}

	sess, err := session.NewActiveSession(r, id, s.deps.Config(), time.Now().UTC(),
		identity.CredentialsTypeRecoveryCode, identity.AuthenticatorAssuranceLevel1)
	if err != nil {
		return s.retryRecoveryFlowWithError(w, r, f.Type, err)
	}

	switch {
	case f.Type == flow.TypeBrowser:
		// TODO: How does this work with Mobile?
		if err := s.deps.SessionManager().UpsertAndIssueCookie(ctx, w, r, sess); err != nil {
			return s.retryRecoveryFlowWithError(w, r, f.Type, err)
		}
	case f.Type == flow.TypeAPI:
		if err := s.deps.SessionPersister().UpsertSession(r.Context(), sess); err != nil {
			return s.retryRecoveryFlowWithError(w, r, f.Type, err)
		}
		f.ContinueWith = append(f.ContinueWith, flow.NewContinueWithSetToken(sess.Token))
	}

	sf, err := s.deps.SettingsHandler().NewFlow(w, r, sess.Identity, f.Type)
	if err != nil {
		return s.retryRecoveryFlowWithError(w, r, f.Type, err)
	}

	returnToURL := s.deps.Config().SelfServiceFlowRecoveryReturnTo(r.Context(), nil)
	returnTo := ""
	if returnToURL != nil {
		returnTo = returnToURL.String()
	}
	sf.RequestURL, err = x.TakeOverReturnToParameter(f.RequestURL, sf.RequestURL, returnTo)
	if err != nil {
		return s.retryRecoveryFlowWithError(w, r, f.Type, err)
	}

	if err := s.deps.RecoveryExecutor().PostRecoveryHook(w, r, f, sess); err != nil {
		return s.retryRecoveryFlowWithError(w, r, f.Type, err)
	}

	config := s.deps.Config()

	sf.UI.Messages.Set(text.NewRecoverySuccessful(time.Now().Add(config.SelfServiceFlowSettingsPrivilegedSessionMaxAge(ctx))))
	if err := s.deps.SettingsFlowPersister().UpdateSettingsFlow(r.Context(), sf); err != nil {
		return s.retryRecoveryFlowWithError(w, r, f.Type, err)
	}

	switch {
	case f.Type.IsAPI():
		f.ContinueWith = append(f.ContinueWith, flow.NewContinueWithSettingsUI(sf))
		s.deps.Writer().Write(w, r, f)
	case x.IsJSONRequest(r):
		s.deps.Writer().WriteError(w, r, flow.NewBrowserLocationChangeRequiredError(sf.AppendTo(s.deps.Config().SelfServiceFlowSettingsUI(r.Context())).String()))
	default:
		http.Redirect(w, r, sf.AppendTo(s.deps.Config().SelfServiceFlowSettingsUI(r.Context())).String(), http.StatusSeeOther)
	}

	return errors.WithStack(flow.ErrCompletedByStrategy)
}

func (s *Strategy) recoveryUseCode(w http.ResponseWriter, r *http.Request, body *recoverySubmitPayload, f *recovery.Flow) error {
	ctx := r.Context()
	code, err := s.deps.RecoveryCodePersister().UseRecoveryCode(ctx, f.ID, body.Code)
	if errors.Is(err, ErrCodeNotFound) {
		f.UI.Messages.Clear()
		f.UI.Messages.Add(text.NewErrorValidationRecoveryCodeInvalidOrAlreadyUsed())
		if err := s.deps.RecoveryFlowPersister().UpdateRecoveryFlow(ctx, f); err != nil {
			return s.retryRecoveryFlowWithError(w, r, f.Type, err)
		}

		// No error
		return nil
	} else if err != nil {
		return s.retryRecoveryFlowWithError(w, r, f.Type, err)
	}

	recovered, err := s.deps.IdentityPool().GetIdentity(ctx, code.IdentityID, identity.ExpandDefault)
	if err != nil {
		return s.HandleRecoveryError(w, r, f, nil, err)
	}

	// mark address as verified only for a self-service flow
	if code.CodeType == RecoveryCodeTypeSelfService {
		if err := s.markRecoveryAddressVerified(w, r, f, recovered, code.RecoveryAddress); err != nil {
			return s.HandleRecoveryError(w, r, f, body, err)
		}
	}

	return s.recoveryIssueSession(w, r, f, recovered)
}

func (s *Strategy) retryRecoveryFlowWithMessage(w http.ResponseWriter, r *http.Request, ft flow.Type, message *text.Message) error {
	s.deps.Logger().
		WithRequest(r).
		WithField("message", message).
		Debug("A recovery flow is being retried because a validation error occurred.")

	ctx := r.Context()
	config := s.deps.Config()

	f, err := recovery.NewFlow(config, config.SelfServiceFlowRecoveryRequestLifespan(ctx), s.deps.CSRFHandler().RegenerateToken(w, r), r, s, ft)
	if err != nil {
		return err
	}

	f.UI.Messages.Add(message)
	if err := s.deps.RecoveryFlowPersister().CreateRecoveryFlow(ctx, f); err != nil {
		return err
	}

	if x.IsJSONRequest(r) {
		http.Redirect(w, r, urlx.CopyWithQuery(urlx.AppendPaths(config.SelfPublicURL(ctx),
			recovery.RouteGetFlow), url.Values{"id": {f.ID.String()}}).String(), http.StatusSeeOther)
	} else {
		http.Redirect(w, r, f.AppendTo(config.SelfServiceFlowRecoveryUI(ctx)).String(), http.StatusSeeOther)
	}

	return errors.WithStack(flow.ErrCompletedByStrategy)
}

func (s *Strategy) retryRecoveryFlowWithError(w http.ResponseWriter, r *http.Request, ft flow.Type, recErr error) error {
	s.deps.Logger().
		WithRequest(r).
		WithError(recErr).
		Debug("A recovery flow is being retried because a validation error occurred.")

	ctx := r.Context()
	config := s.deps.Config()

	if expired := new(flow.ExpiredError); errors.As(recErr, &expired) {
		return s.retryRecoveryFlowWithMessage(w, r, ft, text.NewErrorValidationRecoveryFlowExpired(expired.ExpiredAt))
	}

	f, err := recovery.NewFlow(config, config.SelfServiceFlowRecoveryRequestLifespan(ctx), s.deps.CSRFHandler().RegenerateToken(w, r), r, s, ft)
	if err != nil {
		return err
	}
	if err := f.UI.ParseError(node.CodeGroup, recErr); err != nil {
		return err
	}
	if err := s.deps.RecoveryFlowPersister().CreateRecoveryFlow(ctx, f); err != nil {
		return err
	}

	if x.IsJSONRequest(r) {
		http.Redirect(w, r, urlx.CopyWithQuery(urlx.AppendPaths(config.SelfPublicURL(ctx),
			recovery.RouteGetFlow), url.Values{"id": {f.ID.String()}}).String(), http.StatusSeeOther)
	} else {
		http.Redirect(w, r, f.AppendTo(config.SelfServiceFlowRecoveryUI(ctx)).String(), http.StatusSeeOther)
	}

	return errors.WithStack(flow.ErrCompletedByStrategy)
}

// recoveryHandleFormSubmission handles the submission of an Email for recovery
func (s *Strategy) recoveryHandleFormSubmission(w http.ResponseWriter, r *http.Request, f *recovery.Flow, body *recoverySubmitPayload) error {
	if len(body.Email) == 0 {
		return s.HandleRecoveryError(w, r, f, body, schema.NewRequiredError("#/email", "email"))
	}

	ctx := r.Context()
	config := s.deps.Config()

	if err := flow.EnsureCSRF(s.deps, r, f.Type, config.DisableAPIFlowEnforcement(ctx), s.deps.GenerateCSRFToken, body.CSRFToken); err != nil {
		return s.HandleRecoveryError(w, r, f, body, err)
	}

	if err := s.deps.RecoveryCodePersister().DeleteRecoveryCodesOfFlow(ctx, f.ID); err != nil {
		return s.HandleRecoveryError(w, r, f, body, err)
	}

	if err := s.deps.CodeSender().SendRecoveryCode(ctx, f, identity.VerifiableAddressTypeEmail, body.Email); err != nil {
		if !errors.Is(err, ErrUnknownAddress) {
			return s.HandleRecoveryError(w, r, f, body, err)
		}
		// Continue execution
	}

	// re-initialize the UI with a "clean" new state
	f.UI = &container.Container{
		Method: "POST",
		Action: flow.AppendFlowTo(urlx.AppendPaths(s.deps.Config().SelfPublicURL(r.Context()), recovery.RouteSubmitFlow), f.ID).String(),
	}

	f.UI.SetCSRF(s.deps.GenerateCSRFToken(r))

	f.Active = sqlxx.NullString(s.RecoveryNodeGroup())
	f.State = recovery.StateEmailSent
	f.UI.Messages.Set(text.NewRecoveryEmailWithCodeSent())
	f.UI.Nodes.Append(node.NewInputField("code", nil, node.CodeGroup, node.InputAttributeTypeText, node.WithRequiredInputAttribute).
		WithMetaLabel(text.NewInfoNodeLabelVerifyOTP()),
	)
	f.UI.Nodes.Append(node.NewInputField("method", s.RecoveryNodeGroup(), node.CodeGroup, node.InputAttributeTypeHidden))

	f.UI.
		GetNodes().
		Append(node.NewInputField("method", s.RecoveryStrategyID(), node.CodeGroup, node.InputAttributeTypeSubmit).
			WithMetaLabel(text.NewInfoNodeLabelSubmit()))

	f.UI.Nodes.Append(node.NewInputField("email", body.Email, node.CodeGroup, node.InputAttributeTypeSubmit).
		WithMetaLabel(text.NewInfoNodeResendOTP()),
	)
	if err := s.deps.RecoveryFlowPersister().UpdateRecoveryFlow(r.Context(), f); err != nil {
		return s.HandleRecoveryError(w, r, f, body, err)
	}

	return nil
}

func (s *Strategy) markRecoveryAddressVerified(w http.ResponseWriter, r *http.Request, f *recovery.Flow, id *identity.Identity, recoveryAddress *identity.RecoveryAddress) error {
	var address *identity.VerifiableAddress
	for idx := range id.VerifiableAddresses {
		va := id.VerifiableAddresses[idx]
		if va.Value == recoveryAddress.Value {
			address = &va
			break
		}
	}

	if address != nil && !address.Verified { // can it be that the address is nil?
		address.Verified = true
		verifiedAt := sqlxx.NullTime(time.Now().UTC())
		address.VerifiedAt = &verifiedAt
		address.Status = identity.VerifiableAddressStatusCompleted
		if err := s.deps.PrivilegedIdentityPool().UpdateVerifiableAddress(r.Context(), address); err != nil {
			return s.HandleRecoveryError(w, r, f, nil, err)
		}
	}

	return nil
}

func (s *Strategy) HandleRecoveryError(w http.ResponseWriter, r *http.Request, flow *recovery.Flow, body *recoverySubmitPayload, err error) error {
	if flow != nil {
		email := ""
		if body != nil {
			email = body.Email
		}

		flow.UI.SetCSRF(s.deps.GenerateCSRFToken(r))
		flow.UI.GetNodes().Upsert(
			node.NewInputField("email", email, node.CodeGroup, node.InputAttributeTypeEmail, node.WithRequiredInputAttribute).
				WithMetaLabel(text.NewInfoNodeInputEmail()),
		)
	}

	return err
}

type recoverySubmitPayload struct {
	Method    string `json:"method" form:"method"`
	Code      string `json:"code" form:"code"`
	CSRFToken string `json:"csrf_token" form:"csrf_token"`
	Flow      string `json:"flow" form:"flow"`
	Email     string `json:"email" form:"email"`
}

func (s *Strategy) decodeRecovery(r *http.Request) (*recoverySubmitPayload, error) {
	var body recoverySubmitPayload

	compiler, err := decoderx.HTTPRawJSONSchemaCompiler(recoveryMethodSchema)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	if err := s.dx.Decode(r, &body, compiler,
		decoderx.HTTPDecoderUseQueryAndBody(),
		decoderx.HTTPKeepRequestBody(true),
		decoderx.HTTPDecoderAllowedMethods("POST"),
		decoderx.HTTPDecoderSetValidatePayloads(true),
		decoderx.HTTPDecoderJSONFollowsFormFormat(),
	); err != nil {
		return nil, errors.WithStack(err)
	}

	return &body, nil
}
