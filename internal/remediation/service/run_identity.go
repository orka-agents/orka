package service

import (
	"context"
	"errors"

	"k8s.io/apimachinery/pkg/types"
)

var errRunIdentityChanged = errors.New("remediation run identity changed")

type runIdentity struct {
	namespace string
	id        string
	uid       types.UID
}

type runIdentityContextKey struct{}

func bindRunIdentity(ctx context.Context, identity runIdentity) context.Context {
	return context.WithValue(ctx, runIdentityContextKey{}, identity)
}

func checkRunIdentity(ctx context.Context, namespace, id string, uid types.UID) error {
	expected, fenced := ctx.Value(runIdentityContextKey{}).(runIdentity)
	if fenced && (expected.namespace != namespace || expected.id != id || expected.uid == "" || expected.uid != uid) {
		return errRunIdentityChanged
	}
	return nil
}

func (s *Coordinator) requireCurrentRun(ctx context.Context, expected *run) error {
	if err := checkRunIdentity(ctx, expected.state.Namespace, expected.state.ID, expected.object.UID); err != nil {
		return err
	}
	current, err := s.load(ctx, expected.state.ID)
	if err != nil {
		return err
	}
	if current.object.UID != expected.object.UID {
		return errRunIdentityChanged
	}
	return nil
}
