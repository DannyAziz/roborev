//go:build !unix

package agent

import "errors"

func mkfifo(string) error { return errors.New("mkfifo unsupported") }
