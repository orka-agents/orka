/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package taskwebhook

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	SecretKeyCurrent  = "current"
	SecretKeyPrevious = "previous"

	defaultKeyCacheTTL = 15 * time.Second
	mintAttempts       = 3
)

var ErrSigningUnavailable = errors.New("webhook signing key is unavailable")

type KeySource interface {
	SigningKeys(ctx context.Context) (KeySet, error)
	PublishedKeys(ctx context.Context) (KeySet, error)
}

type StaticKeySource struct{ Keys KeySet }

func (s StaticKeySource) SigningKeys(context.Context) (KeySet, error) {
	if s.Keys.Current == nil {
		return KeySet{}, ErrSigningUnavailable
	}
	return s.Keys, nil
}

func (s StaticKeySource) PublishedKeys(context.Context) (KeySet, error) { return s.Keys, nil }

type SecretKeySource struct {
	Reader    ctrlclient.Reader
	Writer    ctrlclient.Writer
	Namespace string
	Name      string
	CacheTTL  time.Duration
	Now       func() time.Time

	lock      chan struct{}
	lockOnce  sync.Once
	cached    KeySet
	cachedAt  time.Time
	hasCached bool
}

func (s *SecretKeySource) SigningKeys(ctx context.Context) (KeySet, error) {
	if s == nil {
		return KeySet{}, ErrSigningUnavailable
	}
	keys, err := s.load(ctx, s.Writer != nil)
	if err != nil {
		return KeySet{}, err
	}
	if keys.Current == nil {
		return KeySet{}, ErrSigningUnavailable
	}
	return keys, nil
}

func (s *SecretKeySource) PublishedKeys(ctx context.Context) (KeySet, error) {
	return s.load(ctx, false)
}

func (s *SecretKeySource) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

var keyLoadTimeout = 10 * time.Second

func (s *SecretKeySource) load(ctx context.Context, mint bool) (KeySet, error) {
	if s == nil || s.Reader == nil || strings.TrimSpace(s.Namespace) == "" || strings.TrimSpace(s.Name) == "" {
		return KeySet{}, ErrSigningUnavailable
	}
	ttl := s.CacheTTL
	if ttl <= 0 {
		ttl = defaultKeyCacheTTL
	}
	s.lockOnce.Do(func() { s.lock = make(chan struct{}, 1) })
	select {
	case s.lock <- struct{}{}:
	case <-ctx.Done():
		return KeySet{}, fmt.Errorf("%w: waiting for the signing key cache: %v", ErrSigningUnavailable, ctx.Err())
	}
	defer func() { <-s.lock }()
	if s.hasCached && s.now().Sub(s.cachedAt) < ttl && (!mint || s.cached.Current != nil) {
		return s.cached, nil
	}
	readCtx, cancel := context.WithTimeout(ctx, keyLoadTimeout)
	defer cancel()
	keys, err := s.read(readCtx, mint)
	if err != nil {
		return KeySet{}, err
	}
	s.cached, s.cachedAt, s.hasCached = keys, s.now(), true
	return keys, nil
}

func (s *SecretKeySource) read(ctx context.Context, mint bool) (KeySet, error) {
	key := ctrlclient.ObjectKey{Namespace: s.Namespace, Name: s.Name}
	for range mintAttempts {
		secret := &corev1.Secret{}
		err := s.Reader.Get(ctx, key, secret)
		if apierrors.IsNotFound(err) {
			if !mint {
				return KeySet{}, nil
			}
			if createErr := s.create(ctx); createErr != nil && !apierrors.IsAlreadyExists(createErr) {
				return KeySet{}, fmt.Errorf("%w: create signing Secret: %v", ErrSigningUnavailable, apierrors.ReasonForError(createErr))
			}
			continue
		}
		if err != nil {
			return KeySet{}, fmt.Errorf("%w: read signing Secret: %v", ErrSigningUnavailable, apierrors.ReasonForError(err))
		}
		keys, parseErr := parseSecretKeys(secret)
		if parseErr != nil {
			return KeySet{}, parseErr
		}
		if keys.Current != nil || !mint {
			return keys, nil
		}
		pemKey, genErr := generatePEM()
		if genErr != nil {
			return KeySet{}, genErr
		}
		if secret.Data == nil {
			secret.Data = map[string][]byte{}
		}
		secret.Data[SecretKeyCurrent] = pemKey
		if updateErr := s.Writer.Update(ctx, secret); updateErr != nil && !apierrors.IsConflict(updateErr) {
			return KeySet{}, fmt.Errorf("%w: store minted key: %v", ErrSigningUnavailable, apierrors.ReasonForError(updateErr))
		}
	}
	return KeySet{}, fmt.Errorf("%w: signing Secret kept changing", ErrSigningUnavailable)
}

func (s *SecretKeySource) create(ctx context.Context) error {
	pemKey, err := generatePEM()
	if err != nil {
		return err
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      s.Name,
			Namespace: s.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "orka",
				"app.kubernetes.io/component":  "task-webhook-signing",
			},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{SecretKeyCurrent: pemKey},
	}
	return s.Writer.Create(ctx, secret)
}

func parseSecretKeys(secret *corev1.Secret) (KeySet, error) {
	var keys KeySet
	if raw := secret.Data[SecretKeyCurrent]; len(raw) > 0 {
		current, err := ParsePrivateKeyPEM(raw)
		if err != nil {
			return KeySet{}, fmt.Errorf("%w: %q: %v", ErrSigningUnavailable, SecretKeyCurrent, err)
		}
		keys.Current = current
	}
	if raw := secret.Data[SecretKeyPrevious]; len(raw) > 0 {
		previous, err := ParsePrivateKeyPEM(raw)
		if err != nil {
			return KeySet{}, fmt.Errorf("%w: %q: %v", ErrSigningUnavailable, SecretKeyPrevious, err)
		}
		keys.Previous = previous
	}
	if keys.Current == nil && keys.Previous != nil {
		return KeySet{}, fmt.Errorf("%w: %q is set without %q", ErrSigningUnavailable, SecretKeyPrevious, SecretKeyCurrent)
	}
	return keys, nil
}

func generatePEM() ([]byte, error) {
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("%w: generate key: %v", ErrSigningUnavailable, err)
	}
	return EncodePrivateKeyPEM(private)
}
