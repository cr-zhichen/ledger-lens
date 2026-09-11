package fault

import "errors"

// Error is safe to print; causes, credentials and upstream response bodies are omitted.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Exit    int    `json:"-"`
}

func (e *Error) Error() string { return e.Message }

func New(code, message string, exit int) *Error {
	return &Error{Code: code, Message: message, Exit: exit}
}

func Is(err error, code string) bool {
	var target *Error
	return errors.As(err, &target) && target.Code == code
}

func Public(err error) *Error {
	var target *Error
	if errors.As(err, &target) {
		return target
	}
	return New("INTERNAL_ERROR", "本地操作失败", 1)
}

func Invalid(message string) *Error { return New("INVALID_ARGUMENT", message, 2) }
