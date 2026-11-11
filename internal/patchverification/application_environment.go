package patchverification

import (
	"errors"
	"regexp"
	"slices"
	"strings"
)

var applicationEnvironmentName = regexp.MustCompile(`^[A-Z_][A-Z0-9_]{0,63}$`)

func validateApplicationSubjectEnvironment(values map[string]string) error {
	if len(values) > 8 {
		return ErrLimit
	}
	for name, value := range values {
		if !applicationEnvironmentName.MatchString(name) || value != "{{namespace}}" ||
			applicationReservedEnvironment(name) {
			return errors.New("application subject environment accepts only bounded operator-owned namespace bindings")
		}
	}
	return nil
}

func applicationReservedEnvironment(name string) bool {
	for _, prefix := range []string{
		"ORKA_", "POD_", "KUBERNETES_", "GO", "CGO", "LD_", "DYLD_", "GCONV_", "LC_", "SSL_", "TLS_",
		"GIT_", "SSH_", "PYTHON", "RUBY", "PERL", "JAVA", "JDK_", "JRE_", "NODE_", "NPM_", "DOTNET_", "COMPLUS_",
		"BASH_", "ZSH_", "KSH_", "MALLOC_", "GLIBC_",
	} {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	for _, fragment := range []string{"TOKEN", "PASSWORD", "SECRET", "CREDENTIAL", "PRIVATE_KEY", "ACCESS_KEY"} {
		if strings.Contains(name, fragment) {
			return true
		}
	}
	if strings.HasSuffix(name, "_PROXY") || strings.HasSuffix(name, "_CA_BUNDLE") ||
		strings.HasSuffix(name, "_CERT_FILE") || strings.HasSuffix(name, "_CERT_DIR") {
		return true
	}
	return slices.Contains([]string{
		"PATH", "HOME", "USER", "LOGNAME", "SHELL", "IFS", "ENV", "SHELLOPTS", "BASHOPTS", "CDPATH",
		"TMP", "TEMP", "TMPDIR", "LANG", "LOCPATH", "KUBECONFIG", "LIBPATH", "SHLIB_PATH", "TZ", "TZDIR",
		"AUTHORIZATION", "REQUEST_METHOD", "CURL_CA_BUNDLE", "REQUESTS_CA_BUNDLE",
	}, name)
}

// ApplicationSubjectEnvironmentVariables never expands ambient environment or
// model-provided values. The only supported value is the exact owned namespace.
func ApplicationSubjectEnvironmentVariables(policy *ApplicationPolicy, namespace string) ([]string, error) {
	if !applicationName(namespace) {
		return nil, ErrBinding
	}
	if policy == nil {
		return nil, nil
	}
	if err := validateApplicationSubjectEnvironment(policy.SubjectEnvironment); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(policy.SubjectEnvironment))
	for name := range policy.SubjectEnvironment {
		names = append(names, name)
	}
	slices.Sort(names)
	result := make([]string, 0, len(names))
	for _, name := range names {
		result = append(result, name+"="+namespace)
	}
	return result, nil
}
