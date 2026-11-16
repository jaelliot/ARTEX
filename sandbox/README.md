# ARTEX Vagrant sandbox

ARTEX runs in a VM attached only to the `artex-inference` VirtualBox internal
network. A separate gateway VM provides ordinary Vagrant SSH and a restricted
reverse proxy to one configured inference service. The ARTEX VM has no NAT,
bridged, host-only, or other external adapter. The gateway does not route packets:
its firewall drops forwarded traffic and permits the proxy to connect only to the
configured inference IPv4 address on port 443. The proxy accepts only
`GET /v1/models` and `POST /v1/chat/completions` and verifies the upstream TLS
certificate. Its internal HTTP listener is accessible only on the VM-only network.

**Tool isolation is not implemented yet.** ARTEX and its model-invoked tools
currently share an identity and can access the server's local resources. Limit
testing to the model connection test, without starting tasks or invoking tools,
until tool execution has a separate, verified worker boundary.

The gateway and its proxy are part of the trusted boundary. FreeToken is not
application-authenticated, so anyone who can reach the inference proxy can submit
inference requests. The configured inference host may share its IP address with
other services; the proxy restricts the HTTPS hostname and paths, while the
network firewall restricts the IP and port. This is a layered isolation boundary,
not a guarantee against a compromised gateway or hypervisor.

## One-time gateway setup

The default `linux-re-v0` box must already be installed and trusted; it is not
downloaded by these instructions. The gateway uses a reviewed local Caddy binary
with the standard `reverse_proxy` and `request_body` modules. Supply your own
trusted binary; the provisioner copies it into the gateway and records its hash.
On the original host, the locally cached `geckoforge/caddy-cloudflare:2.9.1`
image provided the tested binary. Do not provide the gateway with host folders,
Docker sockets, personal credentials, or a browser profile.

```sh
cd sandbox/inference-gateway
vagrant up --no-provision --provider=virtualbox
python3 provision.py --host inference.example.internal --ip 100.64.0.1 --caddy /path/to/trusted/caddy
cd ..
./start
```

Replace the example host and IP with the inference service's private DNS name and
its current IPv4 address. The address is pinned, so if it changes, stop ARTEX,
disconnect the gateway's internal cable, and run the provisioner again. The
provisioner requires that cable to be disconnected. It configures the gateway's
firewall and proxy, disables Docker and IP forwarding there, restricts SSH to
key authentication, and records the gateway SSH host key. It does not install
packages or download dependencies.

`./start` creates a DHCP service on the dedicated internal network if needed,
verifies the gateway before connecting it, starts ARTEX with Vagrant, and checks
both VMs afterward. Vagrant also runs the gateway check before `up`, `reload`,
and `resume`; a mismatch blocks startup. The verifier checks the live firewall
rules, forwarding state, proxy and SSH settings, pinned DNS, virtual adapters,
port forwards, DHCP scope, and other running VMs attached to the internal network.
After a configuration change, run `python3 verify.py` before executing ARTEX.
Do not bypass a failed check.

## Use

Normal Vagrant SSH works after `./start`:

```sh
cd sandbox
vagrant ssh
```

Inside ARTEX, configure the **OpenAI-compatible (no API key)** profile with:

- Base URL: `http://192.0.2.1:8080/v1`
- Model: the model served by your inference endpoint
- API key: none

The proxy connects to the pinned upstream over HTTPS. Set the context window to
the inference server's actual capacity. The previously tested FreeToken model
also accepts reasoning effort `none` and non-streaming mode. To access a server
running on the guest's loopback port 8787 from a host browser, open a separate
terminal and keep this standard SSH tunnel running:

```sh
cd sandbox
vagrant ssh -- -N -L 127.0.0.1:18787:127.0.0.1:8787
```

Then visit `http://127.0.0.1:18787`. The tunnel does not give the guest a route
to the host or LAN.

There is no shared checkout. Stage reviewed source and dependencies over SSH;
`git archive HEAD` contains only committed files. The gateway intentionally does
not provide package downloads, DNS, or general internet access. Existing images
and dependencies must be staged separately. Run ARTEX in restricted containers
without a host Docker socket or personal credentials.

## Offline control fixtures

With the cached `golang:1.25.8-bookworm` and `postgres:16-alpine` images already
present in the VM, the scripts below exercise disposable Docker fixtures without
fetching images or reaching the inference service:

```sh
./test-container-controls
./test-postgres-controls
./test-worker-network
```

The first checks the runtime's non-root, read-only, seccomp and AppArmor
settings. The second starts PostgreSQL with temporary storage and dummy
credentials. The third checks a dummy server network and a separate networkless
worker; it requires Docker's isolated bridge gateway mode. All fixture containers
and networks are removed when the scripts finish. These checks do not run ARTEX
or prove that its tools are separated from its server. They do not enforce a
quota on persistent database or application volumes.

`./offline-worker` is a manual prototype that accepts a shell script on standard
input and runs it in a fresh, networkless, resource-limited container with no
host mounts. Its workspace disappears when it exits. The cached default image
contains basic shell tools but not ARTEX's full scanner collection. ARTEX does
not dispatch tools to this worker yet; running the prototype does not make the
server's own tools safe to use.

Use `./stop` from `sandbox/` to halt ARTEX first and then the gateway. A
Vagrant-managed gateway restart leaves its internal cable disconnected until
`./start` verifies and reconnects it.
Snapshots can restore older device settings. Only restore snapshots taken while
the VM was powered off. Use `./restore app SNAPSHOT` or
`./restore gateway SNAPSHOT` from this directory. The script halts the affected
machines before restoring and runs the normal startup checks. Vagrant snapshot restoration is
blocked because it can start a restored machine before the checks run. Saved or
paused VM states are rejected: do not discard or resume them without reviewing
their contents and network configuration. Starting or restoring a VM directly
through VirtualBox bypasses the Vagrant startup checks.
Do not execute the bundled PHP proof-of-concept lab; it creates a privileged
container. These controls do not replace authorization for future audit targets.
