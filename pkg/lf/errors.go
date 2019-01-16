/*
 * LF: Global Fully Replicated Key/Value Store
 * Copyright (C) 2018  ZeroTier, Inc.  https://www.zerotier.com/
 *
 * Licensed under the terms of the MIT license (see LICENSE.txt).
 */

package lf

import "fmt"

// Error indicates a general LF error such as an invalid parameter or state.
type Error string

func (e Error) Error() string { return (string)(e) }

// ErrorRecord indicates an error related to an invalid record or a record failing a check.
type ErrorRecord string

func (e ErrorRecord) Error() string { return (string)(e) }

// ErrorTrappedPanic indicates a panic trapped by recover() and returned as an error.
type ErrorTrappedPanic struct {
	PanicError interface{}
}

func (e ErrorTrappedPanic) Error() string {
	return fmt.Sprintf("trapped unexpected panic: %v", e.PanicError)
}

// ErrorDatabase contains information about a database related problem.
type ErrorDatabase struct {
	// ErrorCode is the error code returned by the C database module.
	ErrorCode int

	// ErrorMessage is an error message supplied by the C code or by Go (optional)
	ErrorMessage string
}

func (e ErrorDatabase) Error() string {
	return fmt.Sprintf("database error: %d (%s)", e.ErrorCode, e.ErrorMessage)
}

// General errors
const (
	ErrorInvalidPublicKey  Error = "invalid public key"
	ErrorInvalidPrivateKey Error = "invalid private key"
	ErrorInvalidParameter  Error = "invalid parameter"
	ErrorIsNil             Error = "invalid parameter (nil not allowed)"
	ErrorIsNotStruct       Error = "invalid parameter (must be struct)"
	ErrorUnsupportedType   Error = "unsupported type"
	ErrorUnsupportedCurve  Error = "unsupported ECC curve (for this purpose)"
	ErrorOutOfRange        Error = "parameter out of range"
	ErrorWharrgarblFailed  Error = "Wharrgarbl proof of work algorithm failed (out of memory?)"
	ErrorIO                Error = "I/O error"
	ErrorMessageIncomplete Error = "message incomplete"
	ErrorIncorrectKey      Error = "incorrect key"
)

// Errors indicating that a record is invalid
const (
	ErrorRecordViolatesSpecialRelavitity ErrorRecord = "record timestamp is in the future"
	ErrorRecordInvalid                   ErrorRecord = "record data invalid"
	ErrorRecordOwnerSignatureCheckFailed ErrorRecord = "owner signature check failed"
	ErrorRecordSelectorClaimCheckFailed  ErrorRecord = "claim signature check failed (key/ID or selector)"
	ErrorRecordInsufficientWork          ErrorRecord = "insufficient work to pay for this record"
	ErrorRecordUnsupportedAlgorithm      ErrorRecord = "unsupported algorithm or type"
)
