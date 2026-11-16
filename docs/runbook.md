# Azure provisioning & cutover runbook

This is not code — it's a checklist you (the account owner) run yourself in
the Azure Portal, a shell with `az`/`func`, and Claude's UI. Provisioning is
manual, same choice as Concept2's: a single-user hobby deployment isn't
worth Bicep/Terraform. None of this can be delegated to a coding agent — it
needs your own Azure subscription, Entra tenant, Apple ID, and Claude
account.

## Part 1: Base resources

- [ ] **Provision base resources** (Azure Portal → Create a resource):
  Resource Group; Storage Account; Function App on a **Flex Consumption**
  plan, **Custom Handler** runtime (there's no Go-specific worker — Go
  always deploys as a self-hosted custom handler, same preview feature
  Concept2 uses for Python); Application Insights; Key Vault. Note the Key
  Vault's name — it goes into `local.settings.json` and the Function App's
  settings below.

- [ ] **Seed the iCloud credential**: with `ICLOUD_KEY_VAULT_NAME` set to
  the real vault name and `az login` completed, run
  `go run ./cmd/seed-credentials` locally once. This runs as *your own*
  signed-in identity, which needs secret **write** access on the vault
  (e.g. `Key Vault Secrets Officer`, or vault owner/admin). It's the only
  thing that ever writes the secret — the deployed Function App only reads
  it (see the Managed Identity step below).

- [ ] **Local `func start` smoke test, against the real vault, before
  deploying**: install [Azure Functions Core
  Tools](https://learn.microsoft.com/en-us/azure/azure-functions/functions-run-local)
  and [Azurite](https://learn.microsoft.com/en-us/azure/storage/common/storage-install-azurite)
  if not already present. Fill in the real vault name in
  `local.settings.json`, build the binary (`go build -o ios-mcp-server
  ./cmd/server`), start Azurite, then run `func start` from the project
  root. Confirm `list_calendars` and `list_events` work end-to-end (a real
  CalDAV call through the Key Vault-backed credential) via any MCP client.

## Part 2: Build and deploy (the Go-specific part)

Unlike Concept2's Python deployment, there's no remote build step (Oryx
pip-install) to go wrong — you ship a pre-compiled static binary, so what
runs in Azure is exactly what you tested locally.

- [ ] **Cross-compile for the Function App's Linux x64 host**, from the
  project root:

  ```bash
  GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o ios-mcp-server ./cmd/server
  ```

- [ ] **Set the Function App's custom-handler app settings** (same two
  gotchas Concept2's runbook hit, since they're generic to the self-hosted
  custom handler preview, not Python-specific):

  ```bash
  az functionapp config appsettings set --name <function-app-name> --resource-group <rg> \
    --settings "AzureWebJobsFeatureFlags=EnableMcpCustomHandlerPreview" \
    "AzureFunctionsJobHost__customHandler__http__DefaultAuthorizationLevel=anonymous" \
    "ICLOUD_KEY_VAULT_NAME=<vault-name>" \
    "ICLOUD_CALENDAR_ALLOWLIST=<comma,separated,calendar,names>" \
    "ICLOUD_DEFAULT_TIMEZONE=America/Chicago"
  ```

  The `DefaultAuthorizationLevel=anonymous` setting matters for the same
  reason it did for Concept2: the custom handler's own `authLevel` gate is
  independent of Easy Auth, and even a fully valid Easy-Auth-approved Entra
  token won't bypass it if this is left unset — every request will 401/403
  with no useful diagnostic.

- [ ] **Deploy**: zip just `host.json` and the compiled `ios-mcp-server`
  binary (not the Go source — it isn't needed at runtime, and leaving it
  out keeps the deployed package small) and push it:

  ```bash
  rm -rf publish && mkdir publish
  cp host.json ios-mcp-server publish/
  (cd publish && zip -r ../publish.zip .)
  az functionapp deployment source config-zip --name <function-app-name> --resource-group <rg> --src publish.zip
  ```

- [ ] **Grant the Function App's Managed Identity read-only Key Vault
  access**: enable a system-assigned Managed Identity (`az functionapp
  identity assign --name <function-app-name> --resource-group <rg>`), then
  grant it `Key Vault Secrets User` (read secret contents only) scoped to
  the vault:

  ```bash
  az role assignment create --role "Key Vault Secrets User" \
    --assignee <managed-identity-principal-id> --scope <vault-resource-id>
  ```

  The server only ever reads the credential secret; writing it is done
  solely by `cmd/seed-credentials` under your own `az login` identity (see
  Part 1), so the Function App doesn't need, and shouldn't have, `Key Vault
  Secrets Officer`.

  RBAC propagation can take a minute or two before the first call succeeds;
  without this, tool calls fail with `DefaultAzureCredential failed to
  retrieve a token`.

## Part 3: Entra ID / Easy Auth (identical shape to Concept2)

- [ ] **Start adding the custom connector in Claude** (web: Settings →
  Connectors → Add custom connector) with your Function App's URL
  (`https://<function-app-name>.azurewebsites.net/mcp`) — expect Dynamic
  Client Registration to fail ("Couldn't register with [server]'s sign-in
  service," offering "add an OAuth Client ID" as a fallback). That's
  expected; don't finish the connector yet.

- [ ] **Create the "Claude" Entra ID App Registration**: Azure Portal →
  Microsoft Entra ID → App registrations → New registration. Single-tenant.
  Redirect URI (type: **Web**) = `https://claude.ai/api/mcp/auth_callback`
  — fixed, documented by Anthropic for every hosted Claude surface.

- [ ] **Custom domain required** — `*.azurewebsites.net` cannot be an
  Application ID URI (Microsoft blocked unverified domains as of Oct 2021;
  without this you'll hit `AADSTS9010010`). Put the Function App behind a
  custom domain you own and have verified in Entra ID:
  1. Register/verify the domain in Entra ID (`POST
     https://graph.microsoft.com/v1.0/domains`, then the TXT-record
     verification dance).
  2. Add a TXT record at `asuid.<subdomain>` with the Function App's domain
     ownership token (`az resource show --ids <site-id> --query
     properties.customDomainVerificationId`).
  3. CNAME `<subdomain>` → `<app>.azurewebsites.net`.
  4. `az functionapp config hostname add --name <app> -g <rg> --hostname
     <subdomain>` once DNS resolves from a public resolver.
  5. `az functionapp config ssl create` + `ssl bind` for a free managed
     certificate.
  6. Add the custom-domain MCP URL as a second `identifierUris` entry on
     the server App Registration, and to Easy Auth's `allowedAudiences`
     (next step).
  7. Use the custom-domain URL — not `azurewebsites.net` — in Claude's
     connector.

- [ ] **Enable Easy Auth**: Function App → Settings → Authentication → Add
  identity provider → Microsoft. Single-tenant, client secret expiration
  180 days. Client application requirement → allow only the "Claude" app's
  Client ID. Unauthenticated requests: **HTTP 401 Unauthorized** (critical —
  the redirect default breaks MCP's discovery flow). Token store: checked.
  **Explicitly set `enabled: false` on every identity provider you aren't
  using** (`apple`, `facebook`, `gitHub`, `google`,
  `legacyMicrosoftAccount`, `twitter`) — a `PUT` to `authsettingsV2` that
  only specifies `azureActiveDirectory` silently leaves these defaulted to
  `enabled: true` with empty registrations, which makes Easy Auth's
  bearer-token validation unreliable. This bit Concept2's rollout and isn't
  Python-specific.

- [ ] **Authorize "Claude" against the server's exposed API**: server App
  Registration → Expose an API → Authorized client applications → add
  "Claude" app's Client ID, check `access_as_user`. Copy the resulting
  scope (`api://<server-app-id>/access_as_user`).

- [ ] **Set Protected Resource Metadata**: Function App → Environment
  variables → `WEBSITE_AUTH_PRM_DEFAULT_WITH_SCOPES` = that scope.

- [ ] **Restrict sign-in to yourself**: server App Registration's
  Enterprise Application → Properties → "Assignment required?" → Yes →
  add your own account.

- [ ] **Verify with a simpler client first**: e.g. VS Code's `MCP: Add
  Server...` pointed at the Function App URL — confirm real Microsoft
  sign-in and a working `tools/list`, before touching Claude's connector.

- [ ] **Finish the Claude connector**: paste the "Claude" app's Client
  ID/Secret into Claude's connector setup, complete the connection, confirm
  sign-in. Reconfigure Claude Desktop to use the same connector.

- [ ] **End-to-end check**: from Claude, exercise all four tools —
  `list_calendars`, `list_events` (confirm a recurring series expands
  correctly for next week), `get_event`, `create_event` — via web, mobile,
  and Desktop.
