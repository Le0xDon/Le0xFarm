# Le0xFarm architecture invariants

## General

- Project: Le0xFarm.
- Language: Go.
- License: Apache-2.0.
- Primary supported OS: Ubuntu 22.04 LTS and newer Ubuntu LTS releases, including 24.04 and 26.04.
- Linux only for current roadmap.
- Do not expand task scope without explicit instruction.

## Components

- Le0xAgent: mining worker component.
- Le0xController: one authoritative controller for one farm.
- Le0xNoda: node/service runtime.
- Le0xBrain: developer-hosted Pro backend.
- Le0xRelay: optional network relay.

## Controller ownership

- One Agent belongs to exactly one Controller at a time.
- Multiple Controllers may exist on the same LAN.
- One Controller represents one FarmID.
- Do not implement active-active Controller or multi-master control.
- Agent mining must continue if Controller is temporarily unavailable.

## Networking

- SSH is NOT part of Le0xFarm runtime/control protocol.
- SSH is allowed only for development/administration.
- LAN discovery: mDNS.
- Main runtime transport: persistent bidirectional gRPC with mTLS.
- Remote Agent/Noda may use explicit Controller host:port.
- Agent does not require an inbound WAN port.
- Self-hosted Le0xRelay is intended to remain available to Free.
- Brain never connects directly to Agent or Noda.
- Valid path: Brain <-> Controller <-> Agent/Noda.

## Identity

- HostID is immutable and independent of hostname, MAC address, IP address and display name.
- Duplicate hostnames are valid.
- Hostname/display name are human-facing only.
- Identity relations must use IDs, not hostnames or IPs.
- One Pro entitlement is bound to one Controller cryptographic identity.

## Hardware

- Inventory and DeviceConfig are separate concepts.
- Inventory = detected physical facts.
- DeviceConfig = how hardware may be used.
- Agent detects hardware automatically.
- GPU identity must prefer stable UUID/PCI identifiers rather than ordinal GPU numbers.
- Initial GPU tuning scope is Power Limit only.
- CPU affinity defaults to auto.
- Privileged operations use a restricted helper; profiles never receive arbitrary sudo.

## Configuration

- Explicit user changes always have highest priority.
- Real configuration conflicts are resolved by the user.
- Built-in/Brain profile modified manually becomes USER_MODIFIED and is never silently overwritten.
- Profile default name equals coin name.
- Automatically added additional profiles use coin-2, coin-3, etc.
- Different automatic profiles are created only when wallet or pool differs.
- Custom names are user-created manually.
- Wallet, Pool and Package are independent entities referenced by IDs from profiles.

## Wallet security

- Free wallet creation is manual public payout address only.
- WalletRef never contains seed, mnemonic or private key.
- Pro may create wallets automatically, but creation occurs locally.
- Le0xBrain never receives seed/mnemonic/private keys.
- Wallet backups use a dedicated asymmetric Wallet Backup keypair.
- Public key encrypts new wallet backups.
- Private recovery key decrypts them.
- Private recovery key is removed from Controller after the user confirms safe external storage.
- Farm/Controller recovery encryption uses a separate Farm Recovery keypair.
- Never reuse Wallet Backup keys, Farm Recovery keys and Brain signing keys.

## Packages and downloads

- Miner/node/wallet artifacts are downloaded and cached by Controller, then distributed to Agent/Noda.
- Ubuntu packages/libraries are installed locally by the Agent/Noda appropriate to its OS version.
- Package artifacts are content-hashed.
- Keep current and previous Package version for rollback.
- Miner releases are detected automatically.
- Default update flow: detect -> download -> verify -> canary -> automatic rollout if successful.
- Failed canary rolls back and blocks that version.

## Runtime

- Controller builds ExecutionPlan.
- Agent executes ExecutionPlan.
- Agent should remain deterministic and avoid making policy decisions.
- User command overrides automation/dev mining/watchdog where they conflict.
- START replaces only conflicting workload on selected resource.
- Unrelated GPU/CPU workloads and user services must remain untouched.
- Typed errors must be preferred over parsing human error strings.

## Multi-GPU

- Package declares whether one miner process supports multiple GPUs.
- If yes, Controller may run one process for multiple devices.
- If no, Controller starts separate miner instances per GPU.
- Each instance has its own PID/ExecutionID/log/API endpoint where needed.
- System must verify activity per GPU where technically possible.
- If mining cannot be verified, report this explicitly instead of claiming MINING.

## Nodes / Le0xNoda

- Le0xNoda binary is installed by default alongside Agent but remains idle until needed.
- Le0xNoda may also be installed alone on a dedicated node host.
- Node topology: shared, per-worker, external.
- One coin+network node instance per Host in current version.
- Controller host may run nodes only through Le0xNoda.
- Deleting Service does not delete blockchain data by default.
- Blockchain data backup is not part of normal Le0xFarm backup.
- Node health should eventually use RPC, peers, height progress and reference/network height.

## Free

- Free has 0% developer mining.
- No artificial limits on number of Agents, profiles, pools, wallets, packages or nodes.
- Free can manually create all runtime entities.
- Free includes local Agent, Controller, Noda, CLI and local Web UI.
- Free remote connectivity supports direct WAN and self-hosted Relay.

## Pro

- Pro uses the same runtime objects as Free.
- Pro adds Le0xBrain services.
- One Pro entitlement per Controller identity.
- Entitlement modes include: contribution, developer, complimentary, test/trial.
- Developer/complimentary entitlement may have contribution_required=false.
- Only Brain can grant/revoke entitlement.
- User cannot disable contribution locally.

## Developer contribution

- Normal Pro contribution = 3%.
- Accounting is per physical mining resource.
- CPU and individual GPUs have independent verified user-mining counters.
- Contribution becomes due after 24 hours of verified user mining for that resource.
- CPU and GPU contribution sessions may run simultaneously or at different times.
- Only confirmed active developer mining counts.
- Download/startup/failure/zero-hash time does not count.
- User action immediately preempts conflicting developer session.
- Developer mining never starts, stops or modifies user nodes/services.
- Developer profile cannot require local Le0xNoda services.
- During dev session missing dependencies are never installed automatically.
- After session Controller may offer installation, only after explicit user approval.
- Developer fallback order: latest -> emergency -> last-known-good.
- CPU emergency profile is XMRig + RandomX + Monero.
- If failure is caused by Le0xBrain/developer infrastructure, that period must not count toward Pro suspension.
- Brain normally verifies contribution pool-side.
- If Brain-side verification itself is unavailable, Controller-signed local accounting may be accepted.
- If contribution debt remains eligible for 72 hours, Pro is suspended.
- Pro returns only after contribution debt is fully cleared.
- Debt/grace time advances only while the corresponding hardware is actually performing verified user mining.

## Backup / recovery

- Recovery backup runs at most once every 24 hours and only if useful state changed.
- Keep last 3 successful recovery backups.
- Recovery backup contains all non-reconstructable state.
- Exclude blockchain data, downloadable artifacts/cache and temporary runtime data.
- Controller can be restored from recovery backup.
- Emergency recovery from Agents must also be supported if Controller and backup are lost.
- Recovery from Agents may be partial and must clearly report unrecoverable history/metadata.

## History / security

- Telemetry retention target: 30 days.
- Audit history must be hash-chained/tamper-evident.
- Support bundle must exclude all secrets.
- Data sent to Brain for diagnosis must be sanitized.
- Untrusted crypto repositories are built/tested in disposable VM isolation, not on production Agent/Controller/Noda.
- Brain uses offline root signing key and replaceable/revocable online signing key.
