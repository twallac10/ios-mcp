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

  In the Function App wizard:
  - On the Authentication tab, choose **Managed identity** rather than
    secrets. The wizard creates one user-assigned identity
    (`<app>-uami`) and grants it the host-storage, deployment-storage and
    Application Insights roles, so no storage keys end up in app settings.
    The same identity reads Key Vault (Part 2).
  - **Secure unique default hostname** (on by default) gives the app a
    hostname like `<app>-<hash>.<region>-01.azurewebsites.net` rather than
    `<app>.azurewebsites.net`. Copy the real **Default domain** from the
    app's Overview page; wherever this runbook says
    `<default-hostname>`, use that value.

  The Key Vault isn't part of the Function App wizard — create it
  separately in the same resource group, with the **Azure role-based
  access control** permission model (Part 2 grants access by role
  assignment, not access policy).

- [ ] **Grant yourself secret write access on the vault**: with the RBAC
  permission model, creating the vault — or owning the subscription —
  does *not* let you read or write its secrets. Assign yourself `Key Vault
  Secrets Officer` scoped to the vault (or use the vault's Access control
  (IAM) → Add role assignment in the portal):

  ```bash
  az role assignment create --role "Key Vault Secrets Officer" \
    --assignee $(az ad signed-in-user show --query id -o tsv) \
    --scope $(az keyvault show --name <vault-name> --query id -o tsv)
  ```

  Allow a minute or two for it to propagate; without it the seed step
  below fails with `Forbidden`.

- [ ] **Generate an iCloud app-specific password**: iCloud's CalDAV
  endpoint won't accept your normal Apple Account password (or any OAuth
  flow) — it needs an app-specific password.
  1. Sign in at [account.apple.com](https://account.apple.com) →
     **Sign-In and Security** → **App-Specific Passwords**. (Apple only
     offers these once two-factor authentication is on for the account.)
  2. Generate one with a recognizable label, e.g. `ios-mcp`, so you can
     revoke exactly this one later.
  3. Copy it immediately — it's shown once, in the form
     `xxxx-xxxx-xxxx-xxxx`. Keep the hyphens. Don't save it anywhere else;
     the next step puts it in Key Vault, and if you lose it before then,
     just revoke it and generate another.

  You'll also need the **Apple ID email** the calendars belong to — the
  primary email of that Apple Account, not an alias.

- [ ] **Seed the iCloud credential**: with `az login` completed, run
  locally, once:

  ```bash
  ICLOUD_KEY_VAULT_NAME=<vault-name> go run ./cmd/seed-credentials
  ```

  It prompts for the Apple ID email, then the app-specific password
  (typed into the prompt, not echoed and never a command-line argument, so
  it stays out of shell history). It writes both, as one JSON secret named
  `icloud-credentials`, to the vault, and prints `Saved.` on success.

  This runs as *your own* signed-in identity, which needs the `Key Vault
  Secrets Officer` role from the previous step. It's the only thing that
  ever writes the secret — the deployed Function App only reads it (see
  the Managed Identity step in Part 2).

  Seeding does **not** check the password against iCloud — a typo is only
  caught by the smoke test below, as an iCloud authentication error. To
  confirm the secret exists without printing its value:

  ```bash
  az keyvault secret show --vault-name <vault-name> --name icloud-credentials \
    --query "{name:name, updated:attributes.updated}"
  ```

  To rotate later: generate a new app-specific password, re-run the same
  command (it overwrites the secret), then revoke the old password at
  account.apple.com. No redeploy needed — the running server re-reads the
  vault the next time iCloud rejects its cached credential.

- [ ] **Local `func start` smoke test, against the real vault, before
  deploying**: install [Azure Functions Core
  Tools](https://learn.microsoft.com/en-us/azure/azure-functions/functions-run-local)
  and [Azurite](https://learn.microsoft.com/en-us/azure/storage/common/storage-install-azurite)
  if not already present. Create `local.settings.json` in the project root
  — it's gitignored, so a fresh clone doesn't have one; copy the template
  from the README's [Local development](../README.md#local-development)
  section — and fill in the real vault name and your calendar allowlist
  (exact display names, as shown in the Calendar app). Build the binary (`go build -o ios-mcp-server
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
  access**: reuse the user-assigned identity the wizard created in Part 1
  (`<app>-uami`) — don't add a system-assigned one as well. From the
  identity's Overview page, note its **Client ID** and **Object
  (principal) ID**.

  Point `DefaultAzureCredential` at it. Without `AZURE_CLIENT_ID`, the
  Azure SDK asks App Service for the *system-assigned* identity, which
  doesn't exist, and every Key Vault read fails:

  ```bash
  az functionapp config appsettings set --name <function-app-name> --resource-group <rg> \
    --settings "AZURE_CLIENT_ID=<uami-client-id>"
  ```

  Set this only on the Function App, never in `local.settings.json` —
  locally, `DefaultAzureCredential` should keep resolving to your `az
  login` session.

  Then grant the identity `Key Vault Secrets User` (read secret contents
  only) scoped to the vault — `--assignee` takes the principal ID, not the
  client ID:

  ```bash
  az role assignment create --role "Key Vault Secrets User" \
    --assignee <uami-principal-id> --scope <vault-resource-id>
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
  (`https://<default-hostname>/mcp`) — expect Dynamic
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
  3. CNAME `<subdomain>` → `<default-hostname>`.
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
