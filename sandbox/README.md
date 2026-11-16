# ARTEX Vagrant sandbox

This directory owns a separate Vagrant VM and state. It follows the reverse
engineering lab's VM boundary: the default host share, forwarded SSH agent,
clipboard, drag-and-drop, and automatic provisioning are disabled. ARTEX and its Docker
containers run inside the guest. The VM is disposable; its data disappears when
the VM is destroyed.

## Network boundary: not yet verified

**Do not run ARTEX or untrusted material in this VM until its effective network
and host-isolation controls have been independently verified.** This Vagrantfile
uses NAT for SSH and connectivity. NAT is not an air gap and can reach external
networks and the host. It requests that VirtualBox disable NAT access to host
loopback, but the reverse-engineering lab observed VirtualBox 7.2.14 fail to
retain that setting. A requested setting is not evidence it is enforced.
On this host, an ARTEX VM booted with `LocalhostReachable: 0`, but the saved
configuration omitted the restriction and VirtualBox reported `1` after
shutdown. The VM has not passed the persistence check.

Before an autonomous run, use an independently enforced network boundary that
limits egress to the authorized target and model endpoint and blocks access to
the host and unrelated networks. Verify the actual VirtualBox settings and
network behavior after every boot or snapshot restore. Guest-only firewall rules
are insufficient against a privileged guest workload. This repository does not
yet provide or validate that external network enforcement.

## Bring-up after network isolation is established

The default `linux-re-v0` is the locally prepared box from the reverse-engineering
lab, not one fetched or built by this project. Check that it is installed and
trusted with `vagrant box list`. If using a different prepared VirtualBox box,
set `ARTEX_VM_BOX` consistently for all Vagrant commands. The guest needs Docker
and Docker Compose. Do not copy host credentials, home directories, Docker
sockets, or real browser profiles into it.

From this directory, create the dedicated VM:

```sh
vagrant up --provider=virtualbox
python3 verify.py
```

The verifier checks the running VM and its saved VirtualBox settings. It fails
closed when a setting is missing or unsafe, including the NAT persistence issue.
A successful result is only a settings check: it does not establish target/model
egress restrictions or host-network isolation. Verify those independently before
staging anything. Run the settings and network checks again after every restart
or snapshot restore.

There is no shared checkout. After verifying isolation, stage a committed source
snapshot from the repository root. This transfers the files in `HEAD`, excluding
uncommitted changes, Git metadata, and ignored runtime data. Review the committed
contents for secrets before transfer. The destination must not already exist; use
a fresh VM for a new snapshot.

```sh
git archive HEAD | (cd sandbox && vagrant ssh -c 'umask 077; mkdir ~/artex-src && tar -xf - -C ~/artex-src')
```

Within the guest, configure secrets for this disposable environment and build
the application using its existing Compose workflow. Building downloads
third-party dependencies; it is not an offline or independently verified build.
Use only credentials and data appropriate for the authorized test.

```sh
cd sandbox
vagrant ssh
cd ~/artex-src
umask 077
cp .env.example .env
# Set a strong POSTGRES_PASSWORD and the required model configuration in .env.
docker compose up -d --build
```

The Compose UI listens on the guest's loopback interface. From another host
terminal, forward it through SSH, then open `http://127.0.0.1:18787`:

```sh
cd sandbox
vagrant ssh -- -N -L 127.0.0.1:18787:127.0.0.1:8787
```

Snapshots retain guest data and secrets. Preserve only reviewed evidence before
resetting or destroying the VM. Do not execute the bundled PHP proof-of-concept
lab: it creates an exposed, privileged container. `vagrant destroy` removes this
VM and its guest data; it does not clean up copies exported to the host.
