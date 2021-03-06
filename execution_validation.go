package main

import (
	"github.com/nu7hatch/gouuid"
)

// Validates the execution of a process

type ExecutionValidation struct {
	Id           string // Unique id
	Fatal        bool   // If matched, should we abort the (sequence of) operation(s)?
	MustContain  bool   // Should this be in there?
	OutputStream int    // 1 = standard output, 2 error output
	Text         string // Text to match
}

// Must contain XYZ
func newExecutionValidation(txt string, fatal bool, mustContain bool, outputStream int) *ExecutionValidation {
	// Validate stream
	if outputStream != 1 && outputStream != 2 {
		return nil
	}

	// Must have text
	if len(txt) < 1 {
		return nil
	}

	// Id
	id, _ := uuid.NewV4()

	return &ExecutionValidation{
		Id:           id.String(),
		Fatal:        true,
		MustContain:  true,
		Text:         txt,
		OutputStream: 1,
	}
}
