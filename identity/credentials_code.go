// Copyright © 2023 Ory Corp
// SPDX-License-Identifier: Apache-2.0

package identity

import (
	"database/sql"
)

type CodeAddressType string

const (
	CodeAddressTypeEmail CodeAddressType = AddressTypeEmail
	CodeAddressTypePhone CodeAddressType = AddressTypePhone
)

// CredentialsOTP represents an OTP code
//
// swagger:model identityCredentialsOTP
type CredentialsCode struct {
	// The type of the address for this code
	AddressType CodeAddressType `json:"address_type"`

	// UsedAt indicates whether and when a recovery code was used.
	UsedAt sql.NullTime `json:"used_at,omitempty"`
}
