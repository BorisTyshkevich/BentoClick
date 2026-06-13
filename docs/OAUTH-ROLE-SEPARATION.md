# Channel-scoped authority for one identity (Option A) — future research

> **Status: research / not implemented.** The implementable-now answer is
> Option C in [`RBAC.md`](RBAC.md) (the LLM authors as a service identity
> `anon_author`). This doc records how Option A — *same identity, authority
> scoped by channel, enforced by ClickHouse* — could work on the Altinity
> Antalya build with Auth0 tuning, and what must be verified first.

## The problem A solves

`user@domain.com` reaches ClickHouse two ways: **directly** (the bentoclick
SPA → real data on prod, full grants) and **indirectly** (the LLM via the MCP
→ must NOT read real data, only author tokenized dashboards). RBAC authorizes
by identity, and both paths are the same email — so a static grant is visible
to both, and worse, a session can `SET ROLE` to **any granted role**, so any
data role granted for the direct path is reachable from the LLM path.

A makes the **token** (which channel issued it) decide the session's roles,
while keeping `currentUser()` = the email. That preserves bentoclick's
`currentUser()`-based ownership and forgery-proofness (A's advantage over C,
which relocates ownership-stamping into the MCP).

## Why the HTTP authenticator can't do A (and Antalya can)

The HTTP external authenticator returns **settings only, no roles** — and
settings can't express "may write `dashboards_raw`, may not read
`claude_otel`" (`readonly` blocks the dashboard write too; there's no
per-database allow setting). So A is impossible on the community
`ch-jwt-verify` path.

The Antalya build (otel runs 26.1 Antalya) instead uses native
`token_processors` + `user_directories`, which map OAuth tokens to ClickHouse
roles in-server. That is the mechanism A needs.

## Mechanism — the two-token model

One human, one Auth0 tenant, **two OAuth clients**:

- **SPA client** (bentoclick frontend, public/PKCE) → token scoped for data.
- **MCP client** (altinity-mcp broker) → token scoped for authoring only.

Auth0 issues a distinct token per client; ClickHouse maps each to a different
role set; `username_claim = email` keeps `currentUser()` identical on both.
The LLM only ever obtains the MCP client's token, and the MCP client is
authorized in Auth0 for the authoring scope only — so it can never acquire a
data-scoped token.

### Two ClickHouse implementation variants

**Variant A1 — audience-routed processors (fits today's config shape).**
The otel `user_directories/token` currently assigns a fixed `<common_roles>`
to every token user. Define **two** token_processors keyed on `audience`,
each with its own `common_roles`:

```xml
<token_processors>
  <bento_data>      <type>openid</type>
    <configuration_endpoint>…/.well-known/openid-configuration</configuration_endpoint>
    <audience>https://clickhouse/data</audience>      </bento_data>
  <bento_authoring> <type>openid</type>
    <configuration_endpoint>…/.well-known/openid-configuration</configuration_endpoint>
    <audience>https://clickhouse/authoring</audience> </bento_authoring>
</token_processors>
<user_directories>
  <token><processor>bento_data</processor>
    <common_roles><bentoclick_anon_viewer_role/><!-- + per-user data roles --></common_roles></token>
  <token><processor>bento_authoring</processor>
    <common_roles><anon_author_role/></common_roles></token>
</user_directories>
```

The SPA client requests `aud=…/data`, the MCP client `aud=…/authoring`; CH
validates each token against the matching processor and assigns that
processor's roles. **Needs verifying**: that CH selects the processor by the
token's `aud` (and rejects a token whose `aud` matches neither). Per-user data
roles can't be expressed in a fixed `common_roles` — so this variant handles
the *split* (data vs authoring) but still needs per-user data grants delivered
some other way (static grants on the email, or variant A2).

**Variant A2 — roles from a token claim (external roles).** One processor; the
session's roles come from a `roles` claim in the JWT (Auth0 sets it per
client). This expresses per-user data roles directly and is the cleaner model
— **if** the Antalya build supports extracting session roles from a claim
(beyond fixed `common_roles`). That capability is the key thing to confirm
against the Antalya build / the `acm/mcp` deployment config; the public
altinity-mcp doc only shows `common_roles`.

## The load-bearing invariant

**The human's ClickHouse identity must have ZERO statically-granted roles.**
All roles must arrive per-session via the matched processor / token claim. If
`data_role` is ever statically granted to the email, a `SET ROLE data_role`
from the LLM-path session re-opens the hole. With no static grants, the
authoring session simply has no `data_role` to activate. This is the one rule
that makes A safe; it must be asserted and tested.

## Which claim drives the role — and how consent gates it

The role driver is the **consent-gated `scope` claim**, scoped by the
**`audience`/resource** indicator — *not* `groups` (IdP org membership: same
on every app, not consent-driven), and *not* the Auth0 RBAC **`permissions`**
claim.

- `audience`/resource selects the API → sets `aud` → (variant A1) picks the CH
  `token_processor`. Each API owns its own scopes.
- `scope` = requested ∩ consented (∩ assigned, when RBAC is on) → (variant A2)
  maps to session roles. This is the claim that reflects "the user allowed
  bentoclick data access, but not the LLM."
- **Trap — avoid `permissions`.** Enabling Auth0 RBAC's "Add Permissions in
  the Access Token" emits a `permissions` claim populated with *all* the
  user's assigned permissions **without consent** — the LLM's token would then
  carry the user's full data permissions. Drive CH off `scope`, never
  `permissions`.

**Consent is the UX layer; the Client Grant is the security ceiling.** Consent
alone isn't a boundary (a user can be tricked into "allow"). The binding
control is the per-client **Client Grant / API access policy**: the LLM client
is authorized to request the authoring scope/audience **only**, so Auth0 won't
issue it a data-scoped token regardless of what it asks or the user clicks
(Auth0 documents Client Grants as "a permission ceiling … regardless of who
logs in"). Ceiling it with the client grant; surface it via consent.

## Auth0 tuning

1. **Two APIs / audiences**: `https://clickhouse/data` and
   `https://clickhouse/authoring`, each with its scopes. `aud` (A1) or `scope`
   (A2) is what CH reads.
2. **Two clients, two ceilings (Client Grants)**: the bentoclick SPA client is
   authorized for the data audience/scope; the LLM client for authoring
   **only**. This ceiling — not consent — is what prevents the LLM from ever
   obtaining data scope.
3. **Consent**: shown per app for the requested scopes. NOTE first-party apps
   skip consent by default — mark third-party or force consent on the API to
   get the user-visible "allow extended access for bentoclick" prompt.
4. **(A2 only) Post-Login Action** if mapping scope→a namespaced roles claim CH
   reads, keyed on `event.client.client_id` / the granted scope:
   ```js
   exports.onExecutePostLogin = async (event, api) => {
     const ns = 'https://clickhouse/';
     const roles = event.client.client_id === MCP_CLIENT_ID
       ? ['anon_author_role']
       : [...event.authorization.roles, 'bentoclick_anon_viewer_role'];
     api.accessToken.setCustomClaim(ns + 'roles', roles); // namespaced claim
   };
   ```
5. **Optional rigor — RFC 8693 actor token.** If the MCP performs token
   exchange, the token carries `act` (actor = MCP, `sub` = user); the Action
   can require `act` *absent* before adding data roles, binding the reduction
   to "a deputy is acting," not just a client id.

## Broker topology caveat (otel runs `broker: true`)

The crisp "two Auth0 apps, two consent screens" model is literal only if the
LLM connector talks to Auth0 **directly**. In otel's broker mode, altinity-mcp
is the OAuth AS to the MCP clients (ChatGPT/Claude via CIMD) and brokers Auth0
upstream — so the LLM path's *Auth0-facing* client is **altinity-mcp's upstream
client**, not a per-connector Auth0 app. Consequences:

- The split becomes: bentoclick SPA = its own Auth0 client (data ceiling);
  altinity-mcp's upstream client = authoring ceiling. The "deny data to the
  LLM" guarantee comes from the **client-grant ceiling on the broker's
  upstream client**.
- The LLM-facing consent the user sees is the **MCP `/authorize` screen**; the
  Auth0 consent is for the broker. Per-connector (ChatGPT vs Claude)
  granularity lives at the MCP/CIMD layer, where all LLM connectors share the
  authoring ceiling.
- Verify: what `audience`/`scope` does altinity-mcp request upstream when
  acting for an LLM client, and can that be pinned to authoring-only per the
  broker config?

## Risks & failure modes

- **Claim / audience spoofing** — mitigated: Auth0 signs the JWT, CH verifies
  against JWKS. The LLM can't forge `aud`/`roles`; the trust root is Auth0's
  key + the per-client scope authorization.
- **Static-grant trap** — the invariant above; the single thing that breaks A.
- **Misconfiguration fails closed** — if the processor/claim mapping is wrong,
  the session gets no roles (NONE) → everything denied, not over-granted.
- **Independent token lifetimes** — a leaked authoring token grants only
  authoring; data and authoring TTLs are separate.

## A (future) vs C (now)

| | A — channel-scoped roles | C — service identity (now) |
|---|---|---|
| Identities | one (email) | two (email + `anon_author`) |
| Ownership | CH `currentUser()` (forgery-proof, unchanged) | MCP-stamped from OAuth sub |
| Real-data guarantee | CH role-from-token + no static grants | CH grants on `anon_author` |
| Cross-user authoring isolation | CH (the email identity) | MCP-enforced scoping |
| New work | Antalya role mapping + Auth0 tuning | altinity-mcp: server-set owner + scoped reads |
| Build dependency | Antalya `token_processors` (+ verify external-roles) | any build |

**Migration**: ship C now (no IdP dependency). Move to A when the Antalya
role-mapping path is verified and Auth0 is tuned — A is strictly nicer
(ownership stays CH-enforced, nothing pushed into the MCP). They can coexist:
the MCP path can keep using `anon_author` even after A lands for richer
deployments.

## Open research questions

1. Does the Antalya build support **session roles from a token claim**
   (variant A2), or only fixed `<common_roles>` per processor (variant A1)?
   Check the Antalya build docs and the live `user_directories` config in
   `acm/mcp` (the deployment source of truth).
2. Does CH select the `token_processor` by the token's **`aud`** (variant A1),
   and reject a token whose audience matches no processor?
3. How is otel's current `user_directories/token/common_roles` wired today,
   and what static grants (if any) do JWT-provisioned users carry — i.e. how
   far is the deployment from the zero-static-grants invariant?
4. Is RFC 8693 token exchange (the `act` claim) available on the Auth0 tier in
   use, and is it worth the rigor over per-`client_id` logic?
5. In broker mode, what `audience`/`scope` does altinity-mcp request from Auth0
   when acting for an LLM client, and can the broker pin it to authoring-only
   (the client-grant ceiling on the broker's upstream client)?
6. Are bentoclick + the LLM connector first-party (consent skipped) or
   third-party in the Auth0 tenant — and where does the LLM-facing consent
   actually render (MCP `/authorize` vs Auth0)?

## References

- altinity-mcp `docs/oauth_authorization.md` — `token_processors` /
  `user_directories` config (the otel Bearer-auth path).
- [ClickHouse JWT auth (#17270)](https://github.com/ClickHouse/ClickHouse/issues/17270),
  [JWT in clickhouse-client (#62829)](https://github.com/ClickHouse/ClickHouse/pull/62829).
- [Auth0: roles/permissions via Actions](https://support.auth0.com/center/s/article/add-roles-and-permissions-to-the-id-token-using-actions),
  [Auth0 custom claims with Actions](https://auth0.com/blog/adding-custom-claims-to-id-token-with-auth0-actions/),
  [Auth0 scopes & claims use cases](https://auth0.com/docs/get-started/apis/scopes/sample-use-cases-scopes-and-claims).
