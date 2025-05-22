// Copyright (C) 2019 Nicola Murino
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published
// by the Free Software Foundation, version 3.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program. If not, see <https://www.gnu.org/licenses/>.

package util

import (
	"errors"
	"fmt"
)

const (
	templateLoadErrorHints = "Try setting the absolute templates path in your configuration file " +
		"or specifying the config directory adding the `-c` flag to the serve options. For example: " +
		"sftpgo serve -c \"<path to dir containing the default config file and templates directory>\""
)

// MaxRecursion defines the maximum number of allowed recursions
const MaxRecursion = 1000

// errors definitions
var (
	ErrValidation       = NewValidationError("")
	ErrNotFound         = NewRecordNotFoundError("")
	ErrMethodDisabled   = NewMethodDisabledError("")
	ErrGeneric          = NewGenericError("")
	ErrRecursionTooDeep = errors.New("recursion too deep")
)

// ValidationError raised if input data is not valid
type ValidationError struct {
	err string
}

// Validation error details
func (e *ValidationError) Error() string {
	return fmt.Sprintf("Validation error: %s", e.err)
}

// GetErrorString returns the unmodified error string
func (e *ValidationError) GetErrorString() string {
	return e.err
}

// Is reports if target matches
func (e *ValidationError) Is(target error) bool {
	_, ok := target.(*ValidationError)
	return ok
}

// NewValidationError returns a validation errors
func NewValidationError(errorString string) *ValidationError {
	return &ValidationError{
		err: errorString,
	}
}

// RecordNotFoundError raised if a requested object is not found
type RecordNotFoundError struct {
	err string
}

func (e *RecordNotFoundError) Error() string {
	return fmt.Sprintf("not found: %s", e.err)
}

// Is reports if target matches
func (e *RecordNotFoundError) Is(target error) bool {
	_, ok := target.(*RecordNotFoundError)
	return ok
}

// NewRecordNotFoundError returns a not found error
func NewRecordNotFoundError(errorString string) *RecordNotFoundError {
	return &RecordNotFoundError{
		err: errorString,
	}
}

// MethodDisabledError raised if a method is disabled in config file.
// For example, if user management is disabled, this error is raised
// every time a user operation is done using the REST API
type MethodDisabledError struct {
	err string
}

// Method disabled error details
func (e *MethodDisabledError) Error() string {
	return fmt.Sprintf("Method disabled error: %s", e.err)
}

// Is reports if target matches
func (e *MethodDisabledError) Is(target error) bool {
	_, ok := target.(*MethodDisabledError)
	return ok
}

// NewMethodDisabledError returns a method disabled error
func NewMethodDisabledError(errorString string) *MethodDisabledError {
	return &MethodDisabledError{
		err: errorString,
	}
}

// GenericError raised for not well categorized error
type GenericError struct {
	err string
}

func (e *GenericError) Error() string {
	return e.err
}

// Is reports if target matches
func (e *GenericError) Is(target error) bool {
	_, ok := target.(*GenericError)
	return ok
}

// NewGenericError returns a generic error
func NewGenericError(errorString string) *GenericError {
	return &GenericError{
		err: errorString,
	}
}
