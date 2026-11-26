package publisher

import (
	"bytes"
	"crypto/rand"
	"crypto/sha512"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"golang.org/x/crypto/ssh"
)

const (
	sshSigMagicPrefix = "SSHSIG"
	sshSigVersion     = uint32(1)
	sshSigNamespace   = "git"
	sshSigHashAlg     = "sha512"
	sshSigArmorHeader = "-----BEGIN SSH SIGNATURE-----"
	sshSigArmorFooter = "-----END SSH SIGNATURE-----"
	sshSigWrapWidth   = 70
)

func ValidateCommitIdentity(name, email string) (string, string, error) {
	resolvedName := strings.TrimSpace(name)
	if resolvedName == "" {
		resolvedName = CommitAuthorName
	}
	resolvedEmail := strings.TrimSpace(email)
	if resolvedEmail == "" {
		resolvedEmail = CommitAuthorEmail
	}
	if err := validateCommitIdentityPart("commit author name", resolvedName); err != nil {
		return "", "", err
	}
	if err := validateCommitIdentityPart("commit author email", resolvedEmail); err != nil {
		return "", "", err
	}
	if strings.ContainsAny(resolvedEmail, " \t") {
		return "", "", invalid("commit author email", "must not contain whitespace")
	}
	return resolvedName, resolvedEmail, nil
}

func validateCommitIdentityPart(field, value string) error {
	if value == "" || len(value) > 256 || !utf8.ValidString(value) {
		return invalid(field, "must be non-empty UTF-8 at most 256 bytes")
	}
	if strings.ContainsAny(value, "\r\n\x00<>") {
		return invalid(field, "must not contain control characters or angle brackets")
	}
	return nil
}

func ParseSSHSigningKey(privateKeyPEM []byte) (ssh.Signer, string, error) {
	if len(bytes.TrimSpace(privateKeyPEM)) == 0 {
		return nil, "", invalid("commit signing key", "private key must not be empty")
	}
	signer, err := ssh.ParsePrivateKey(privateKeyPEM)
	if err != nil {
		if _, ok := errors.AsType[*ssh.PassphraseMissingError](err); ok {
			return nil, "", invalid("commit signing key", "passphrase-protected SSH signing keys are not supported")
		}
		return nil, "", invalid("commit signing key", "failed to parse OpenSSH private key")
	}
	if signer == nil || signer.PublicKey() == nil {
		return nil, "", invalid("commit signing key", "signing key has no public key")
	}
	switch signer.PublicKey().Type() {
	case ssh.KeyAlgoED25519, ssh.KeyAlgoRSA:
	default:
		return nil, "", invalid("commit signing key", "unsupported SSH signing key type %q (only ssh-ed25519 and ssh-rsa are supported for deterministic commit signing)", signer.PublicKey().Type())
	}
	return signer, ssh.FingerprintSHA256(signer.PublicKey()), nil
}

func SSHSigningKeyFingerprint(privateKeyPEM []byte) (string, error) {
	_, fingerprint, err := ParseSSHSigningKey(privateKeyPEM)
	return fingerprint, err
}

func SignGitCommitPayload(signer ssh.Signer, unsignedPayload []byte) (string, error) {
	if signer == nil || signer.PublicKey() == nil {
		return "", invalid("commit signing key", "signer is required")
	}
	toSign := sshSigSignedData(unsignedPayload)
	var sig *ssh.Signature
	var err error
	if algoSigner, ok := signer.(ssh.AlgorithmSigner); ok {
		algo := sshSigAlgorithmForKey(signer.PublicKey().Type())
		sig, err = algoSigner.SignWithAlgorithm(rand.Reader, toSign, algo)
	} else {
		sig, err = signer.Sign(rand.Reader, toSign)
	}
	if err != nil {
		return "", fmt.Errorf("sign commit with SSH key: %w", err)
	}
	blob := encodeSSHSigBlob(signer.PublicKey(), sig)
	return armorSSHSig(blob), nil
}

func VerifyGitCommitSignature(publicKey ssh.PublicKey, unsignedPayload []byte, armoredSig string) error {
	if publicKey == nil {
		return invalid("commit signing key", "public key is required")
	}
	blob, err := dearmorSSHSig(armoredSig)
	if err != nil {
		return err
	}
	sigPubKey, sig, err := decodeSSHSigBlob(blob)
	if err != nil {
		return err
	}
	if !bytes.Equal(sigPubKey.Marshal(), publicKey.Marshal()) {
		return fmt.Errorf("SSH signature public key does not match expected signing key")
	}
	toSign := sshSigSignedData(unsignedPayload)
	if err := publicKey.Verify(toSign, sig); err != nil {
		return fmt.Errorf("SSH commit signature verification failed: %w", err)
	}
	return nil
}

func sshSigAlgorithmForKey(keyType string) string {
	switch keyType {
	case ssh.KeyAlgoRSA:
		return ssh.KeyAlgoRSASHA512
	default:
		return keyType
	}
}

func sshSigSignedData(message []byte) []byte {
	hash := sha512.Sum512(message)
	var buf bytes.Buffer
	buf.WriteString(sshSigMagicPrefix)
	writeSSHString(&buf, []byte(sshSigNamespace))
	writeSSHString(&buf, nil)
	writeSSHString(&buf, []byte(sshSigHashAlg))
	writeSSHString(&buf, hash[:])
	return buf.Bytes()
}

func encodeSSHSigBlob(pubKey ssh.PublicKey, sig *ssh.Signature) []byte {
	var buf bytes.Buffer
	buf.WriteString(sshSigMagicPrefix)
	var version [4]byte
	binary.BigEndian.PutUint32(version[:], sshSigVersion)
	buf.Write(version[:])
	writeSSHString(&buf, pubKey.Marshal())
	writeSSHString(&buf, []byte(sshSigNamespace))
	writeSSHString(&buf, nil)
	writeSSHString(&buf, []byte(sshSigHashAlg))
	writeSSHString(&buf, ssh.Marshal(sig))
	return buf.Bytes()
}

func decodeSSHSigBlob(blob []byte) (ssh.PublicKey, *ssh.Signature, error) {
	if len(blob) < len(sshSigMagicPrefix)+4 || string(blob[:len(sshSigMagicPrefix)]) != sshSigMagicPrefix {
		return nil, nil, fmt.Errorf("invalid SSHSIG magic prefix")
	}
	rest := blob[len(sshSigMagicPrefix):]
	version := binary.BigEndian.Uint32(rest[:4])
	if version != sshSigVersion {
		return nil, nil, fmt.Errorf("unsupported SSHSIG version %d", version)
	}
	rest = rest[4:]
	pubBytes, rest, err := readSSHString(rest)
	if err != nil {
		return nil, nil, fmt.Errorf("read SSHSIG public key: %w", err)
	}
	namespace, rest, err := readSSHString(rest)
	if err != nil || string(namespace) != sshSigNamespace {
		return nil, nil, fmt.Errorf("invalid SSHSIG namespace")
	}
	reserved, rest, err := readSSHString(rest)
	if err != nil || len(reserved) != 0 {
		return nil, nil, fmt.Errorf("invalid SSHSIG reserved field")
	}
	hashAlg, rest, err := readSSHString(rest)
	if err != nil || string(hashAlg) != sshSigHashAlg {
		return nil, nil, fmt.Errorf("unsupported SSHSIG hash algorithm")
	}
	sigBytes, rest, err := readSSHString(rest)
	if err != nil || len(rest) != 0 {
		return nil, nil, fmt.Errorf("invalid SSHSIG signature payload")
	}
	pubKey, err := ssh.ParsePublicKey(pubBytes)
	if err != nil {
		return nil, nil, fmt.Errorf("parse SSHSIG public key: %w", err)
	}
	var sig ssh.Signature
	if err := ssh.Unmarshal(sigBytes, &sig); err != nil {
		return nil, nil, fmt.Errorf("parse SSHSIG signature: %w", err)
	}
	return pubKey, &sig, nil
}

func writeSSHString(buf *bytes.Buffer, data []byte) {
	var length [4]byte
	binary.BigEndian.PutUint32(length[:], uint32(len(data))) //nolint:gosec // bounded SSH wire string
	buf.Write(length[:])
	buf.Write(data)
}

func readSSHString(in []byte) ([]byte, []byte, error) {
	if len(in) < 4 {
		return nil, nil, fmt.Errorf("truncated SSH string length")
	}
	n := int(binary.BigEndian.Uint32(in[:4]))
	if n < 0 || len(in)-4 < n {
		return nil, nil, fmt.Errorf("truncated SSH string body")
	}
	return in[4 : 4+n], in[4+n:], nil
}

func armorSSHSig(blob []byte) string {
	encoded := base64.StdEncoding.EncodeToString(blob)
	var b strings.Builder
	b.WriteString(sshSigArmorHeader)
	b.WriteByte('\n')
	for i := 0; i < len(encoded); i += sshSigWrapWidth {
		end := min(i+sshSigWrapWidth, len(encoded))
		b.WriteString(encoded[i:end])
		b.WriteByte('\n')
	}
	b.WriteString(sshSigArmorFooter)
	return b.String()
}

func dearmorSSHSig(armored string) ([]byte, error) {
	lines := strings.Split(strings.TrimSpace(armored), "\n")
	if len(lines) < 3 || lines[0] != sshSigArmorHeader || lines[len(lines)-1] != sshSigArmorFooter {
		return nil, fmt.Errorf("malformed SSH signature armor")
	}
	joined := strings.Join(lines[1:len(lines)-1], "")
	return base64.StdEncoding.DecodeString(joined)
}

func buildUnsignedCommitObject(treeOID, parentOID, authorName, authorEmail, date, message string) []byte {
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "tree %s\n", treeOID)
	fmt.Fprintf(&buf, "parent %s\n", parentOID)
	fmt.Fprintf(&buf, "author %s <%s> %s\n", authorName, authorEmail, date)
	fmt.Fprintf(&buf, "committer %s <%s> %s\n", authorName, authorEmail, date)
	buf.WriteByte('\n')
	buf.WriteString(message)
	return buf.Bytes()
}

func buildSignedCommitObject(treeOID, parentOID, authorName, authorEmail, date, armoredSig, message string) []byte {
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "tree %s\n", treeOID)
	fmt.Fprintf(&buf, "parent %s\n", parentOID)
	fmt.Fprintf(&buf, "author %s <%s> %s\n", authorName, authorEmail, date)
	fmt.Fprintf(&buf, "committer %s <%s> %s\n", authorName, authorEmail, date)
	buf.WriteString("gpgsig ")
	sigLines := strings.Split(armoredSig, "\n")
	for i, line := range sigLines {
		if i > 0 {
			buf.WriteByte(' ')
		}
		buf.WriteString(line)
		buf.WriteByte('\n')
	}
	buf.WriteByte('\n')
	buf.WriteString(message)
	return buf.Bytes()
}
