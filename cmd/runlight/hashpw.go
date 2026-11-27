package main

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/dannyjp49/runlight/internal/dashboard"
)

// printPasswordHash reads one line from stdin and prints its PBKDF2 hash. The
// password never appears in argv, so it stays out of ps and shell history.
func printPasswordHash() error {
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && line == "" {
		return errors.New("no password on stdin")
	}
	pw := strings.TrimRight(line, "\r\n")
	if len(pw) < 8 {
		return errors.New("the password must have at least 8 characters")
	}
	h, err := dashboard.HashPassword(pw)
	if err != nil {
		return err
	}
	fmt.Println(h)
	return nil
}
