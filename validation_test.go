package main

import (
	"context"
	"testing"
)

func TestEXE(t *testing.T) {
	if err := validateExe(context.Background(), nil, "/usr/bin/lua5.1"); err != nil {
		t.Fatal(err)
	}
}