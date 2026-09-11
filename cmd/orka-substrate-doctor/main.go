package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"

	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/workspace"
)

func main() {
	var cfg workspace.SubstrateConfig
	var template, publicKeyFrom string
	flag.StringVar(&cfg.APIEndpoint, "api-endpoint", os.Getenv("ORKA_SUBSTRATE_API_ENDPOINT"),
		"native ate-api TLS address")
	flag.StringVar(&cfg.APICAFile, "ca-file", os.Getenv("ORKA_SUBSTRATE_API_CA_FILE"), "trusted server CA bundle")
	flag.StringVar(&cfg.APICertFile, "cert-file", os.Getenv("ORKA_SUBSTRATE_API_CERT_FILE"),
		"rotating client certificate PEM")
	flag.StringVar(&cfg.APIKeyFile, "key-file", os.Getenv("ORKA_SUBSTRATE_API_KEY_FILE"),
		"rotating client private-key PEM")
	flag.StringVar(&cfg.APIBearerTokenFile, "bearer-token-file", os.Getenv("ORKA_SUBSTRATE_API_BEARER_TOKEN_FILE"),
		"rotating control bearer file instead of mTLS")
	flag.StringVar(&cfg.Atespace, "atespace", "", "native Atespace")
	flag.StringVar(&template, "template", "", "operator-owned native infrastructure ActorTemplate")
	flag.StringVar(&publicKeyFrom, "public-key-from", "",
		"print only the direct workspace bootstrap public key derived from this secret file")
	flag.Parse()
	if publicKeyFrom != "" {
		data, err := os.ReadFile(publicKeyFrom)
		if err != nil {
			fmt.Fprintln(os.Stderr, "cannot read bootstrap signing secret file")
			os.Exit(1)
		}
		key, err := harnessv2.WorkspaceBootstrapPublicKey(strings.TrimSpace(string(data)))
		if err != nil {
			fmt.Fprintln(os.Stderr, "invalid bootstrap signing secret")
			os.Exit(1)
		}
		fmt.Println(key)
		return
	}
	report := workspace.DiagnoseSubstrate(context.Background(), cfg, template)
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(report); err != nil {
		os.Exit(1)
	}
	if !report.Ready {
		os.Exit(1)
	}
}
