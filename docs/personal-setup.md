# Personal setup: one proxy at home, reached over Tailscale

This guide sets up this fork of CLIProxyAPI the way Theo (t3.gg) runs it:
- one always-on machine on your home network holds your Claude and Codex subscription logins;
- every other machine reaches it over Tailscale;
- Claude Code and Codex send their requests through it.

The outcome:
- **One residential IP:** all subscription traffic leaves from your home connection.
- **Logins in one place:** OAuth tokens are refreshed by the proxy.
- **Smarter routing:** requests use the account whose weekly quota resets soonest.

It covers this fork's defaults (session affinity, Codex WebSockets, the `soonest-reset` strategy) and its dashboard. Upstream CLIProxyAPI behaves differently in those places.

## Read this first

Theo's warnings, in his words where possible:

- **No VPS, no VPN.** "I highly advise you do not sign in to Claude Code or Codex, but especially Claude Code, on a VPS." Anthropic is aggressive about sign-ins and requests from server IP ranges (AWS, Hetzner and so on). Run the proxy at home and do the OAuth sign-in with any VPN turned off. Don't send subscription traffic straight from several machines either; route it through this one box.
- **Claude subscriptions only through Claude Code.** "I highly recommend you only ever use Claude models through your Claude subs in Claude Code over the proxy." Don't point other tools at Claude models through this proxy.
- **Never serve other people's traffic.** Running your own prompts and agents on your own code is the point. "If you're using this as an alternative to the API to serve user traffic, you deserve your ban." Don't put a product, a public endpoint or anyone else's requests on these accounts, and don't share your tailnet access to the proxy.

This may still be against the providers' terms and can get accounts banned. You run it at your own risk.

## 1. The always-on machine

Any machine that stays on at home works: a desktop, a mini PC, an old laptop. It needs Go 1.26+ to build, and it must not sleep. On macOS, turn off sleep in System Settings → Energy, or run `sudo pmset -a sleep 0`.

Build the fork:

```sh
git clone https://github.com/LucasMouraFranco/CLIProxyAPI.git
cd CLIProxyAPI
go build -o cli-proxy-api ./cmd/server
cp config.example.yaml config.yaml
```

Edit `config.yaml`. The settings that matter for this setup:

```yaml
server:
  host: "127.0.0.1"   # loopback only; Tailscale exposes it (step 2)
  port: 8317

management:
  allow-remote: false
  secret-key: "<long random string>"  # hashed on first start; this is the dashboard login
  panel-github-repository: "https://github.com/LucasMouraFranco/Cli-Proxy-API-Management-Center"

access:
  api-keys:
    - "<long random string>"  # what Claude Code and Codex send; replace all example keys

routing:
  strategy: soonest-reset
  # session-affinity: true   # already the default in this fork

oauth:
  auth-dir: "~/.cli-proxy-api"  # where the login tokens are stored
```

Notes:
- **Replace every `your-api-key-*` example key.** The proxy API stays disabled while any of them is present. Generate keys with `openssl rand -hex 32`.
- **API keys are optional.** Theo runs without any, because only devices on his tailnet can reach the proxy. If you leave `api-keys` empty, any device on your tailnet can use your subscriptions.
- **Codex WebSockets need no config line.** They are on by default for Codex OAuth logins; see [What this fork changes](#what-this-fork-changes).

Run it once in the foreground to check it starts:

```sh
./cli-proxy-api -config config.yaml
```

### Keep it running

Linux, as a systemd user service (`~/.config/systemd/user/cli-proxy-api.service`):

```ini
[Unit]
Description=CLIProxyAPI
After=network-online.target

[Service]
WorkingDirectory=%h/CLIProxyAPI
ExecStart=%h/CLIProxyAPI/cli-proxy-api -config %h/CLIProxyAPI/config.yaml
Restart=on-failure

[Install]
WantedBy=default.target
```

```sh
systemctl --user enable --now cli-proxy-api
loginctl enable-linger "$USER"   # keep it running while you're logged out
```

macOS, as a launchd agent (`~/Library/LaunchAgents/dev.cliproxyapi.plist`; use your own paths):

```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>dev.cliproxyapi</string>
  <key>WorkingDirectory</key><string>/Users/you/CLIProxyAPI</string>
  <key>ProgramArguments</key>
  <array>
    <string>/Users/you/CLIProxyAPI/cli-proxy-api</string>
    <string>-config</string>
    <string>/Users/you/CLIProxyAPI/config.yaml</string>
  </array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
</dict>
</plist>
```

```sh
launchctl load ~/Library/LaunchAgents/dev.cliproxyapi.plist
```

## 2. Expose it only over Tailscale

Install Tailscale on the proxy machine and on every machine that will use it, and sign them into the same tailnet. In the Tailscale admin console, enable MagicDNS and HTTPS certificates. Then, on the proxy machine:

```sh
tailscale serve --bg http://127.0.0.1:8317
tailscale serve status
```

The proxy is now at `https://<machine>.<tailnet>.ts.net` for devices on your tailnet and nowhere else. The proxy itself only listens on `127.0.0.1`, so your LAN can't reach it either.

- **Never use `tailscale funnel` for this.** Funnel publishes the service to the internet.
- **To restrict access further,** add a Tailscale access rule that lets only your own devices reach this machine.
- **`tailscale serve` connects to the proxy from `127.0.0.1`,** so the proxy treats tailnet requests as local. That's why `allow-remote: false` works here. The management key is what protects the dashboard, so keep it long. Leave `server.trusted-proxies` empty.

Alternative: skip `tailscale serve` and bind the proxy to the machine's Tailscale address (`server.host` set to the output of `tailscale ip -4`). You then use plain `http://<machine>.<tailnet>.ts.net:8317`, and the dashboard needs `management.allow-remote: true`, because requests now arrive from tailnet addresses.

## 3. Log in your Claude and Codex accounts

Each login adds one auth file to `oauth.auth-dir`. Repeat for every account. Do the browser part with any VPN turned off.

**From the dashboard (easiest on a headless machine):**
1. Open the dashboard (step 4) and go to **OAuth Login**.
2. Start the Claude or Codex login and open the link in your browser.
3. Sign in. The provider then redirects to a `http://localhost:...` address, which won't load on your laptop. That's expected.
4. Copy that whole URL from the address bar, paste it into **Callback URL**, and submit.

**From a shell on the proxy machine:**

```sh
./cli-proxy-api -config config.yaml -claude-login -no-browser
./cli-proxy-api -config config.yaml -codex-login -no-browser
./cli-proxy-api -config config.yaml -codex-device-login   # Codex without a callback
```

`-no-browser` prints the link instead of opening it. The OAuth redirect goes to `localhost` on the machine running the browser, port 54545 for Claude and 1455 for Codex. If you sign in from your laptop over SSH, forward that port first, for example `ssh -L 54545:localhost:54545 <proxy-machine>`. `-codex-device-login` needs no forwarding.

The proxy refreshes the tokens itself. If a Claude login stops working (Theo sees this every few days), log that account in again the same way.

## 4. Use the fork's dashboard

Open `https://<machine>.<tailnet>.ts.net/management.html` and sign in with the management key. **Quota Management** shows:
- per-provider totals, such as "409% of 500%" across five Claude accounts;
- each account's 5-hour and 7-day windows with reset countdowns;
- the account `soonest-reset` will pick next;
- per-account prompt-cache reads and writes.

Emails are masked until you press **Show emails**.

`panel-github-repository` makes the proxy download the panel (`management.html`) from the latest GitHub release of the dashboard fork, `LucasMouraFranco/Cli-Proxy-API-Management-Center`. It checks the asset's digest and looks for updates every few hours. Two catches:
- **The dashboard fork needs a release.** GitHub turns workflows off on new forks: enable them once in that repository's **Actions** tab. After that, pushing a `v*` tag runs `.github/workflows/release.yml`, which builds and publishes `management.html`.
- **Fallback to upstream's panel.** If the proxy finds no release and has no local copy yet, it falls back to upstream's hosted panel at `cpamc.router-for.me`. That's not this fork's dashboard.

To use the fork's dashboard before there's a release, build it yourself and put it in `static/` next to `config.yaml`:

```sh
git clone https://github.com/LucasMouraFranco/Cli-Proxy-API-Management-Center.git
cd Cli-Proxy-API-Management-Center
bun install && bun run build
mkdir -p ~/CLIProxyAPI/static
cp dist/index.html ~/CLIProxyAPI/static/management.html
```

The proxy keeps a local copy when it can't find a release. If you previously ran upstream, replace the upstream copy that's already in `static/`.

## 5. Point Claude Code and Codex at the proxy

`<proxy>` below is `https://<machine>.<tailnet>.ts.net`, and `CLIPROXY_API_KEY` holds one of your `access.api-keys`. With no API keys configured, any non-empty value works.

### Claude Code

Claude Code reads two variables:

```sh
export ANTHROPIC_BASE_URL="<proxy>"
export ANTHROPIC_AUTH_TOKEN="$CLIPROXY_API_KEY"
```

To keep your normal Claude Code untouched, make it a separate command with its own home directory. Add this to `~/.zshrc` or `~/.bashrc`:

```sh
claude-proxy() {
  CLAUDE_CONFIG_DIR="$HOME/.claude-proxy" \
  ANTHROPIC_BASE_URL="<proxy>" \
  ANTHROPIC_AUTH_TOKEN="$CLIPROXY_API_KEY" \
    claude "$@"
}
```

`claude` keeps using `~/.claude` and your own login; `claude-proxy` uses `~/.claude-proxy` and the proxy. Don't copy credentials into `~/.claude-proxy`: the proxy does the authentication.

### Codex

Codex needs a custom model provider. Point it at the proxy's `/v1` and turn on WebSockets, so it uses the proxy's Codex WebSocket path:

```toml
model_provider = "cliproxy"

[model_providers.cliproxy]
name = "CLIProxyAPI"
base_url = "<proxy>/v1"
env_key = "CLIPROXY_API_KEY"
wire_api = "responses"
supports_websockets = true
```

For a separate command, put that file at `~/.codex-proxy/config.toml` and add:

```sh
codex-proxy() {
  CODEX_HOME="$HOME/.codex-proxy" codex "$@"
}
```

`codex` keeps `~/.codex`; `codex-proxy` uses `~/.codex-proxy` and the proxy. If Codex can't open a WebSocket, it falls back to HTTPS on its own.

### Check it

```sh
curl -s "<proxy>/v1/models" -H "Authorization: Bearer $CLIPROXY_API_KEY" | head
```

Then run one prompt through `claude-proxy` and `codex-proxy`, and watch the request appear on that account's row in the dashboard.

## What this fork changes

| Setting | Fork default | Upstream default |
| --- | --- | --- |
| `routing.session-affinity` | `true`: a Claude Code or Codex session (including its subagents) stays on one account and only moves when that account is unavailable, so its prompt cache stays warm. | `false` |
| Codex WebSockets | On for Codex OAuth logins, with automatic fallback to HTTP when the WebSocket can't be opened. Opt an account out with `"websockets": false` in its auth file, or in the dashboard's auth-file editor. Codex API-key entries keep their own `websockets` option, which is off unless you set it. | Off |
| `routing.strategy` | Set `soonest-reset` yourself (see step 1). It sends new sessions to the usable account whose weekly quota resets soonest, so quota doesn't expire unused. It skips accounts that are exhausted, cooling down or at their 5-hour limit. Accounts with unknown resets go last. | `round-robin` |

`soonest-reset` learns each account's reset times from response headers. After a restart, an account only ranks by its real reset once it has served a request; until then it counts as unknown.

## Keeping the fork current

`main` tracks upstream. To pull in upstream fixes:

```sh
git fetch upstream
git switch main && git merge --ff-only upstream/main && git push origin main
go build -o cli-proxy-api ./cmd/server
```

Then restart the service.
