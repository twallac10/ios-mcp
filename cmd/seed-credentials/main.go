// Run this once (and again any time you rotate the app-specific password)
// to store the Apple ID + app-specific password used to authenticate to
// iCloud's CalDAV server in Azure Key Vault.
//
// Usage:
//
//	ICLOUD_KEY_VAULT_NAME=... go run ./cmd/seed-credentials
//
// You must be signed in to Azure (e.g. via `az login`) with write access to
// that vault. This is the Apple-credential equivalent of the Concept2 MCP
// server's bootstrap_auth.py — except there's no browser OAuth flow here,
// since iCloud CalDAV authenticates with a static app-specific password
// (generate one at https://appleid.apple.com, under "App-Specific
// Passwords"), not an OAuth token.
package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/twallac10/ios-mcp/internal/credentials"
)

func main() {
	vaultName := os.Getenv("ICLOUD_KEY_VAULT_NAME")
	if vaultName == "" {
		fmt.Fprintln(os.Stderr, "ICLOUD_KEY_VAULT_NAME must be set")
		os.Exit(1)
	}

	reader := bufio.NewReader(os.Stdin)

	fmt.Print("Apple ID (email): ")
	appleID, err := reader.ReadString('\n')
	if err != nil {
		fmt.Fprintf(os.Stderr, "reading Apple ID: %v\n", err)
		os.Exit(1)
	}
	appleID = strings.TrimSpace(appleID)

	// Typed into the running prompt (visible on screen, but never a
	// command-line argument), so it never lands in shell history, argv,
	// or any log — unlike `az keyvault secret set --value ...`.
	fmt.Print("App-specific password (from appleid.apple.com): ")
	passwordLine, err := reader.ReadString('\n')
	if err != nil {
		fmt.Fprintf(os.Stderr, "reading app-specific password: %v\n", err)
		os.Exit(1)
	}
	password := strings.TrimSpace(passwordLine)

	if appleID == "" || password == "" {
		fmt.Fprintln(os.Stderr, "both the Apple ID and the app-specific password are required")
		os.Exit(1)
	}

	store, err := credentials.NewStore(vaultName)
	if err != nil {
		fmt.Fprintf(os.Stderr, "connecting to Key Vault %q: %v\n", vaultName, err)
		os.Exit(1)
	}

	ctx := context.Background()
	if err := store.Save(ctx, &credentials.Credentials{AppleID: appleID, AppSpecificPassword: password}); err != nil {
		fmt.Fprintf(os.Stderr, "saving credentials to Key Vault: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("Saved. The server will pick this up on its next credential load (or immediately, if it's never loaded credentials yet).")
}
