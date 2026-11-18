//go:build !linux

package main

import (
	"context"
	"errors"
)

func runBoundedValidationGit([]string) error {
	return errors.New("isolated public-source preparation requires Linux")
}

func runValidationSourcePreparation(context.Context, []string) error {
	return errors.New("isolated public-source preparation requires Linux")
}
