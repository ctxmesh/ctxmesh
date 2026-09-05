# Running and validating ctxmesh locally

This is the loop for bringing ctxmesh up on a laptop, signing in through a real login flow,
and — the part most guides skip — **checking that it actually works** rather than assuming a
page that rendered means a platform that runs.

Nothing here needs Okta, Azure AD, or any external identity provider.

## What you get

A kind cluster running the control plane, a local OIDC provider (Dex) with one demo user, and
the console reachable in a browser. Agents reach `Ready` and are addressable.

**What you do not get without more setup:** agents that *answer*. That needs a real model
provider key. Reaching an agent and getting a real completion out of it are different
problems, and only the first is covered here.

## Bring it up

```sh
make -C harness dev-up          # creates the cluster and installs the platform
make -C harness local-oidc      # installs Dex and wires the API server to trust it
```

`local-oidc` is separate on purpose: the API server's OIDC flags are set when the cluster is
*created* and cannot be added afterwards, while everything else is re-runnable. If you see

```
the API server has no --oidc-issuer-url
```

your cluster predates the OIDC support — recreate it with `dev-up`.

## Trust the local CA — do this before your first login

The API server accepts only `https://` for an OIDC issuer, so Dex is served with a
**self-signed certificate**. Your browser does not trust it, and this bites in a way worth
understanding:

- The **redirect** to Dex shows a certificate warning you can click through.
- The **token exchange** afterwards is a `fetch()` from the console — and a `fetch` to an
  untrusted certificate **fails with no prompt at all**. The login appears to hang or bounce
  back to the sign-in page with no error.

**The one-command way out: `mkcert`.** If `mkcert` is installed (`brew install mkcert`), the
bring-up uses it automatically and there is nothing to accept — it writes its CA into the
system trust store *and* into Firefox's own store. That second half matters: Firefox does not
read the macOS keychain, so trusting a hand-rolled CA there leaves Firefox refusing the issuer
with no visible reason.

So visit the issuer once and accept the warning, which registers the exception for that
origin:

```
https://dex.127-0-0-1.sslip.io:8443/.well-known/openid-configuration
```

You should see a JSON document. Alternatively, add `~/.ctxmesh/oidc-certs/ca.crt` to your
system trust store, which avoids the warning entirely.

## Sign in

Open the console, click **Sign in with SSO**, and use:

| | |
|---|---|
| user | `admin@ctxmesh.local` |
| password | `ctxmesh-dev` |

The demo identity is bound to the `ctxmesh-operator` persona, so you land with real
permissions rather than an empty console.

**Token login still works** and is the fallback whenever SSO is not configured:

```sh
kubectl -n default create token uie2e-admin --duration=24h
```

## Validate it — five checks that fail honestly

Running these beats trusting the UI, because most of the failure modes below render as
something that *looks* fine.

**1. The whole login chain, in one command.**

```sh
make -C harness tier2 SLICE=local-login
```

It proves Dex issues, the API server *authenticates* the token as `oidc:admin@ctxmesh.local`,
and RBAC *authorises* it — and denies what the persona should not have. Authentication alone
proves nothing: a token that authenticates into zero permissions is the symptom of a claim or
prefix mismatch, and it looks exactly like a broken login.

**2. The console answers.** `curl -s localhost:9090/api/authconfig` should report
`"oidcEnabled": true`. If it says `false`, the BFF is missing one of **three** env vars —
`OIDC_ENABLED`, `OIDC_ISSUER`, `OIDC_CLIENT_ID`. It advertises SSO only with all three, and
with any missing the login page quietly offers token entry instead.

**3. Agents are Ready.** `kubectl get agentdeployments -A` — `READY=True` means reconciled and
serving.

**4. An agent's URL opens.** Take the `URL` column and open it. If it does not resolve, the
cluster is not publishing host ports — recreate with `dev-up`.

**5. The console tells the truth about what it does not know.** On the Cost page an unpriced
figure renders as **—**, never `$0.00`. A zero there would mean the cost pipeline is
reporting a number it never measured.

## When it goes wrong

**"Unauthorized" on every request after signing in.** The API server could not reach the
issuer. Check:

```sh
kubectl -n kube-system logs -l component=kube-apiserver --tail=20 | grep oidc
```

`oidc authenticator: initializing plugin` repeating every ten seconds means the API server
cannot fetch Dex's discovery document. The usual cause is DNS: the API server runs on the
node's network and reads the **node's** `/etc/hosts`, and kubelet copies that file into the
pod when the container is **created** — so an entry added after the API server started is
invisible to it. Re-run `make -C harness local-oidc`, which restarts it in the right order.

**Signed in, but permitted nothing.** Authentication worked and no RBAC binding matches your
identity. Check the exact subject name:

```sh
kubectl get clusterrolebinding ctxmesh-oidc-demo -o yaml
```

The `oidc:` prefix must match the API server's `--oidc-username-prefix` exactly. A mismatch
here is silent — you are a valid user that no binding mentions.

**The login bounces back to the sign-in page.** Almost always the untrusted certificate, above.

**A `.localhost` hostname that will not resolve to the cluster.** It cannot. Resolvers
special-case `*.localhost` to loopback (RFC 6761) and `/etc/hosts` cannot override it. That is
why the issuer uses a `sslip.io` name.

## Using a real identity provider instead

Dex is optional. If your cluster's API server already trusts an IdP, point the console
straight at it — no Dex involved:

```sh
helm upgrade ctxmesh … \
  --set bff.oidcEnabled=true \
  --set bff.oidcIssuer=https://your-idp.example.com \
  --set bff.oidcClientId=ctxmesh-console
```

Then bind your IdP's **groups** to the personas, which is what makes access manageable:

```sh
kubectl create clusterrolebinding platform-operators \
  --clusterrole=ctxmesh-operator --group="oidc:platform-engineering"
```

Deploy Dex when your IdP is not OIDC (LDAP, SAML) or when you want one broker in front of
several.
