---
type: spec
status: for the k3s side (devops:#6c44); Tomas confirmed the batch to them on 2026-11-19
title: "cc-sandbox: k3s scope amendments, second batch"
task: cc-sandbox:#326q (milestone cc-sandbox:#2f3e)
date: 2026-11-19
base: k3s main 8aaacc0 (live: K1 cc-sandbox ns, K2, K3, K8 RBAC, K9, K11, watchdog; check 46 PASS)
---

[Index](README.md) › Unified model › [5. Build plan](cc-unified-model-build-plan.md) › k3s amendments

# k3s scope amendments, second batch (2026-11-19)

This is the exact spec for each amendment Tomas confirmed. It builds on the live objects in
`apps/cc-sandbox/` at k3s `8aaacc0` and changes nothing in them unless it says so. **This repo is
public**, so tailnet names and IPs are placeholders (`<…>`). The real values are in the k3s
repo's `artifacts/tailscale-config.json`.

**One change of placement on our side** (doc 5 F4, §4.6): the in-cluster `ccsbx-hostd` moves to
the new namespace, next to the doors, instead of living in `cc-sandbox`. This is why:
- **The PVC.** The hostd PVC (A4) can then live where the launcher can't mount it. A PVC mount is
  namespace-local, and the live K11 rules don't forbid PVC volumes in launcher pods. So
  `cc-sandbox` keeps `persistentvolumeclaims: "0"`.
- **The hub credential.** hostd holds the `spoke:future-two` hub credential. The launcher has
  `pods/exec` on every pod in `cc-sandbox`, so outside `cc-sandbox` it can't exec into hostd.
  This closes the hole flagged in `admission-policy.yaml` ("NOT COVERED HERE") for hostd, as
  well as for the doors.

Roles (label `cc-sandbox/role`):
- `session`: unchanged.
- `door`: now only in `cc-sandbox-doors`.
- `watchdog`: unchanged.
- **new `hostd`**: in `cc-sandbox-doors`.

---

## A1. Namespace `cc-sandbox-doors`

**What lives there** (all created by Flux-managed Deployments, never by the launcher):

| Deployment | Role label | Replicas | Ports (container) | Mounts |
|---|---|---|---|---|
| `ccsp-model` | `door` | 1 | 8080 session, 8090 owner/health | Secrets `cc-sandbox-model`; later `cc-sandbox-model-sub` (after Q13) |
| `ccsp-egress` | `door` | 1 | 8081 session, 8091 owner (decisions) | Secret `cc-sandbox-egress` |
| `ccsp-stub` (e2e realm, C18) | `door` | 1 | 8082 session, 8092 owner | Secret `cc-sandbox-e2e-approver` (a test key) |
| `ccsbx-hostd` | `hostd` | 1, strategy `Recreate` | 7480 session/peer, 7490 owner | Secret `cc-sandbox-hostd-hub`; PVC `ccsbx-hostd-journal` (A4) |

Each Deployment has its own ServiceAccount (`ccsp-model`, `ccsp-egress`, `ccsp-stub`,
`ccsbx-hostd`), with `automountServiceAccountToken: false` and **no Role or RoleBinding**.
ConfigMap `ccsbx-machines` (the trusted-key list, M9) goes in both namespaces. The ghcr pull
secret (K10) goes in both namespaces.

**Namespace object:**

```yaml
apiVersion: v1
kind: Namespace
metadata:
  name: cc-sandbox-doors
  labels:
    cc-sandbox/role: door            # namespace-level marker (pods still carry their own label)
    pod-security.kubernetes.io/enforce: restricted
    pod-security.kubernetes.io/enforce-version: latest
    pod-security.kubernetes.io/audit: restricted
    pod-security.kubernetes.io/audit-version: latest
    pod-security.kubernetes.io/warn: restricted
    pod-security.kubernetes.io/warn-version: latest
```

**Quota and limits:**

```yaml
apiVersion: v1
kind: ResourceQuota
metadata: {name: cc-sandbox-doors-quota, namespace: cc-sandbox-doors}
spec:
  hard:
    requests.cpu: "1"
    limits.cpu: "4"
    requests.memory: 2Gi
    limits.memory: 3Gi
    requests.ephemeral-storage: 2Gi
    limits.ephemeral-storage: 10Gi
    pods: "8"                        # 4 Deployments + rollout surge + a test pod
    persistentvolumeclaims: "1"      # A4 only
    requests.storage: 5Gi
    services.nodeports: "0"
    services.loadbalancers: "0"
---
apiVersion: v1
kind: LimitRange
metadata: {name: cc-sandbox-doors-limits, namespace: cc-sandbox-doors}
spec:
  limits:
    - type: Container
      defaultRequest: {cpu: 100m, memory: 128Mi, ephemeral-storage: 256Mi}
      default:        {cpu: 500m, memory: 512Mi, ephemeral-storage: 1Gi}
      max:            {cpu: "1",  memory: 1Gi,   ephemeral-storage: 2Gi}
```

**NetworkPolicies in `cc-sandbox-doors`:**

```yaml
# 1) default deny
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata: {name: default-deny-all, namespace: cc-sandbox-doors}
spec: {podSelector: {}, policyTypes: [Ingress, Egress]}
---
# 2) sessions -> door session listeners and hostd's session port (never the owner ports)
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata: {name: ingress-from-sessions, namespace: cc-sandbox-doors}
spec:
  podSelector: {matchExpressions: [{key: cc-sandbox/role, operator: In, values: [door, hostd]}]}
  policyTypes: [Ingress]
  ingress:
    - from:
        - namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: cc-sandbox}}
          podSelector: {matchLabels: {cc-sandbox/role: session}}
      ports:
        - {protocol: TCP, port: 8080}
        - {protocol: TCP, port: 8081}
        - {protocol: TCP, port: 8082}
        - {protocol: TCP, port: 7480}
---
# 3) tailnet -> doors and hostd, only through the operator INGRESS proxies made for this
#    namespace (hp/Mac forwarders -> session ports; the owner -> owner ports; peers -> 7480)
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata: {name: ingress-from-tailscale-proxies, namespace: cc-sandbox-doors}
spec:
  podSelector: {matchExpressions: [{key: cc-sandbox/role, operator: In, values: [door, hostd]}]}
  policyTypes: [Ingress]
  ingress:
    - from:
        - namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: tailscale}}
          podSelector: {matchLabels: {tailscale.com/parent-resource-ns: cc-sandbox-doors}}
      ports:
        - {protocol: TCP, port: 8080}
        - {protocol: TCP, port: 8081}
        - {protocol: TCP, port: 8082}
        - {protocol: TCP, port: 8090}
        - {protocol: TCP, port: 8091}
        - {protocol: TCP, port: 8092}
        - {protocol: TCP, port: 7480}
        - {protocol: TCP, port: 7490}
---
# 4) doors -> DNS + public internet tcp/443 minus "us": the live door-egress-internet-443
#    policy MOVES here unchanged (same except-list), plus devops-telegram for the P7 code.
#    (copy spec.egress from apps/cc-sandbox/networkpolicy.yaml policy 4, then add:)
#    - to: [{namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: infra}},
#            podSelector: {matchLabels: {app: devops-telegram}}}]
#      ports: [{protocol: TCP, port: 3000}]
---
# 5) hostd -> the operator EGRESS proxies for the hub (hp:4011) and the peer daemons (hp:7480,
#    Mac:7480). No DNS: the proxy ClusterIPs are passed in as env (see A5).
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata: {name: hostd-egress, namespace: cc-sandbox-doors}
spec:
  podSelector: {matchLabels: {cc-sandbox/role: hostd}}
  policyTypes: [Egress]
  egress:
    - to:
        - namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: tailscale}}
          podSelector: {matchLabels: {tailscale.com/parent-resource-ns: cc-sandbox}}
      ports:
        - {protocol: TCP, port: 4011}
        - {protocol: TCP, port: 7480}
```

**Changes in `cc-sandbox`:**
- Policy 2 (`session-egress-to-doors`) changes its `to:` to `namespaceSelector cc-sandbox-doors` +
  `podSelector role In [door, hostd]`, with ports 8080, 8081, 8082 and 7480.
- Policies 3 and 4 (door ingress and door egress) are **deleted** from `cc-sandbox`, because no
  door lives there any more. Policy 4 moves to the doors namespace as policy 4 above.
- Add to the VAP (A2, rule 11): no pod in `cc-sandbox` may carry `role=door` or `role=hostd`.

**Secrets in `cc-sandbox-doors`** (all SOPS in git, applied by Flux):

| Secret | Key(s) | Mounted only by | Test key? |
|---|---|---|---|
| `cc-sandbox-model` | `ANTHROPIC_API_KEY` (Console-capped, $50/month) | `ccsp-model` | yes (capped) |
| `cc-sandbox-model-sub` | `CLAUDE_CODE_OAUTH_TOKEN` (one-year subscription token) | `ccsp-model` | **no: waits for Q13** |
| `cc-sandbox-egress` | `GITHUB_APP_ID`, `GITHUB_APP_INSTALLATION_ID`, `GITHUB_APP_KEY_B64` | `ccsp-egress` | yes (installed on the testbed repo only) |
| `cc-sandbox-e2e-approver` | the e2e realm's test approver key | `ccsp-stub` | yes |
| `cc-sandbox-hostd-hub` | hostd's `spoke:future-two` hub credential (B6c) | `ccsbx-hostd` | yes (revocable at the hub) |
| ghcr pull secret (K10) | read-only `read:packages` | image pulls | yes |

**Who may read them:**
- **No SA has `get/list/watch` on secrets in `cc-sandbox-doors`.** There is no Role in the
  namespace except the ops SA's (A6), and that one has no secrets.
- **The launcher SA (`cc-sandbox:cc-sandbox-launcher`) has no RoleBinding here at all**: no pods,
  no exec, no secrets.
- Only Flux's controllers and cluster admins can read them. That's inherent; Q13 covers it at
  rest.
- *Optional, recommended:* a VAP bound to `cc-sandbox-doors` that lets each Deployment's pods
  reference only their own Secrets, per the table above.

**Placement:** the VAP binding (`cc-sandbox-pod-guard`) widens its `namespaceSelector` to:
```yaml
    namespaceSelector:
      matchExpressions:
        - {key: kubernetes.io/metadata.name, operator: In, values: [cc-sandbox, cc-sandbox-doors]}
```
so rules 1 and 2 (future-two only) hold for the doors and hostd too. Their Deployments carry the
same nodeSelector and toleration as the sessions.

**Acceptance (Mac kubeconfig):**
- `kubectl auth can-i --as=system:serviceaccount:cc-sandbox:cc-sandbox-launcher {get secrets,create pods,create pods/exec} -n cc-sandbox-doors` → no, no, no
- `kubectl -n cc-sandbox-doors get resourcequota,limitrange,networkpolicy` lists the objects above
- from a probe `role=session` pod in `cc-sandbox`: door `:8080` connects; door `:8090` times out
- from a probe pod without a label in `default`: door `:8080` times out
- a door pod with `nodeName: k3s` is rejected

---

## A2. K11 extension: launcher-created pods

These **add** to the live rules 1–5 in `cc-sandbox-pod-guard`. They apply only when
`variables.byLauncher` is true, unless a rule says otherwise. **Verify CONNECT support in VAP on
k3s v1.33 for rule 12**; if it isn't supported, rule 12 becomes a ValidatingWebhook or is
dropped, given A1 (no door or hostd is left in `cc-sandbox` for the launcher to exec into).

| # | Rule | CEL (sketch) | Why |
|---|---|---|---|
| 6 | no PVC, ephemeral, CSI, hostPath, configMap or downwardAPI volumes; **only `emptyDir`** | `!has(object.spec.volumes) \|\| object.spec.volumes.all(v, has(v.emptyDir))` | the agent gets no data it wasn't handed by the sidecar; closes the PVC hole |
| 7 | every `emptyDir` has a `sizeLimit` | `…all(v, has(v.emptyDir.sizeLimit))` | K9 enforced, not just documented |
| 8 | containers: exactly one, named `fence`; initContainers: at most one, named `side`, `restartPolicy: Always` (a native sidecar); no ephemeralContainers | `object.spec.containers.size() == 1 && object.spec.containers[0].name == 'fence' && (!has(object.spec.initContainers) \|\| (object.spec.initContainers.size() <= 1 && object.spec.initContainers.all(c, c.name == 'side' && has(c.restartPolicy) && c.restartPolicy == 'Always'))) && !has(object.spec.ephemeralContainers)` | doc 5 F2 shape; nothing else rides in a session pod |
| 9 | `shareProcessNamespace` absent or false; `hostUsers` absent or true (PSA covers host*) | `!has(object.spec.shareProcessNamespace) \|\| object.spec.shareProcessNamespace == false` | the fence can't see or ptrace the sidecar's token |
| 10 | `dnsPolicy: None`, and `dnsConfig` with no nameservers or only `127.0.0.1` | `has(object.spec.dnsPolicy) && object.spec.dnsPolicy == 'None' && (!has(object.spec.dnsConfig) \|\| !has(object.spec.dnsConfig.nameservers) \|\| object.spec.dnsConfig.nameservers.all(n, n == '127.0.0.1'))` | defence in depth on top of the no-DNS NetPol |
| 11 | images in digest form from our two repos: `^ghcr\.io/<owner>/(ccsbx-fence\|ccsbx)@sha256:[0-9a-f]{64}$`, for containers and initContainers (**applies to every pod in `cc-sandbox`**, launcher or not); and no pod in `cc-sandbox` carries `role` `door` or `hostd` | `object.spec.containers.all(c, c.image.matches('^ghcr\\.io/<owner>/(ccsbx-fence\|ccsbx)@sha256:[0-9a-f]{64}$'))` + the same for initContainers | no tag-only or foreign image; the watchdog pod is the one exception (it keeps its own image rule) |
| 12 | `pods/exec` and `pods/attach` by the launcher only into pods named `ccsbx-s-*` (CONNECT) | `request.name.startsWith('ccsbx-s-')` on resource `pods`, subresources `exec`, `attach`, operation `CONNECT` | the launcher never execs into a non-session pod (the watchdog, a test pod) |
| 13 | `hostAliases` **allowed** (only IPs, no names checked) | — | the launcher hands the session the proxy and door ClusterIPs this way (no DNS) |
| 14 | pod name prefix `ccsbx-s-` | `object.metadata.name.startsWith('ccsbx-s-')` (`generateName` is resolved before admission) | makes rule 12 meaningful |

**Acceptance** (`T:vap-rejects`, Mac kubeconfig, as the launcher). Each of these pods is rejected:
- a third container;
- a `configMap` volume;
- a `persistentVolumeClaim` volume;
- an emptyDir with no `sizeLimit`;
- `shareProcessNamespace: true`;
- `dnsPolicy: ClusterFirst`;
- an image by tag only;
- an image from another registry;
- `role=door`;
- a name without `ccsbx-s-`.

Also, `kubectl exec` into the watchdog pod is refused. The F2 reference pod is admitted.

**LIVE: k3s `1e76dc1` (devops:#5dxt, merged; PriorityClass `ccsbx-session` and the scoped quotas
exist; k3s's check 139/0).** A tightening of the rules above for launcher-created pods. Only the
last row (`hostUsers`) is still **COMING**:

| Change | Rule | Why |
|---|---|---|
| `priorityClassName: ccsbx-session` is **required** (new class, value 500, `preemptionPolicy: Never`, filled in from the class by the Priority plugin, so the pod doesn't set it); `default-priority` and `low-priority` are for non-launcher pods only | new | an independent k3s review found session pods could pick `system-node-critical` and evict other apps on future-two; 500 + `Never` means a session never preempts anything |
| no `runtimeClassName`, no finalizers | new | no `runtimeClassName`: a session can't pick a runtime with weaker isolation than the default. No finalizers: a session pod can't block its own deletion (cleanup and quota stay in the launcher's hands) |
| `terminationGracePeriodSeconds` at most 60 (unset = 30 is fine) | new | a stopping session can't hold the node's resources for long |
| ephemeral containers (`kubectl debug`) refused in **both namespaces, for everyone** | widens rule 8 | `kubectl debug` would inject a container that bypasses the pod spec's fences (and could reach the sidecar's token); attach is exec only |
| **COMING:** `hostUsers: false` **allowed**; required later, after a test pod proves it on future-two (Tomas decides) | relaxes rule 9, which says absent or true | with `hostUsers: false` a session's uid 0 maps to an unprivileged host uid, so a container escape on future-two's shared kernel lands as nobody, not as host root. Required only after a test pod proves it on future-two (k3s version, idmapped mount of the session volume); requiring it is Tomas's call |

---

## A3. K8 extension: the launcher renews its own token

Today the kubeconfig holds a **non-expiring** legacy token (`cc-sandbox-launcher-token`, type
`kubernetes.io/service-account-token`). Replace it with **TokenRequest** tokens that the launcher
renews itself:

```yaml
# added to Role cc-sandbox-launcher (namespace cc-sandbox)
  - apiGroups: [""]
    resources: ["serviceaccounts/token"]
    resourceNames: ["cc-sandbox-launcher"]
    verbs: ["create"]
---
# a revocation anchor: tokens are bound to it; deleting it invalidates every launcher token
apiVersion: v1
kind: Secret
metadata: {name: cc-sandbox-launcher-anchor, namespace: cc-sandbox}
type: Opaque
data: {}
```

- **Lifetime:** `expirationSeconds: 2592000` (30 days), bound with `boundObjectRef: {kind: Secret,
  apiVersion: v1, name: cc-sandbox-launcher-anchor}`. *Verify:* the apiserver's
  `--service-account-max-token-expiration` (unset by default on k3s) doesn't cap it below 30 days,
  and a bound Secret works for a TokenRequest made through the SA's own subresource.
- **Who renews:** `ccsbx-root` on hp, as uid `ccsbx-keys`, from a daily systemd timer. It renews
  when fewer than 10 days are left, writes the new token into the kubeconfig under
  `/var/lib/ccsbx-keys/` (0600), and alerts by Telegram if renewal fails twice.
- **Bootstrap:** the k3s side mints the first token
  (`kubectl -n cc-sandbox create token cc-sandbox-launcher --duration=720h --bound-object-kind Secret --bound-object-name cc-sandbox-launcher-anchor`),
  builds the kubeconfig, and SOPS-encrypts it to the R10b age key. That's the existing
  `scripts/cc-sandbox-check.sh kubeconfig` flow, with a TokenRequest token instead of the legacy
  one.
- **Then:** after the first successful self-renewal (doc 5 O5 test), the k3s side **deletes** the
  legacy Secret `cc-sandbox-launcher-token` and removes it from `rbac.yaml`.
- **Revoke everything:** `kubectl -n cc-sandbox delete secret cc-sandbox-launcher-anchor`. Flux
  recreates it, and the k3s side re-bootstraps.
- **Acceptance:** `can-i create serviceaccounts/token -n cc-sandbox` → yes for the SA's own name,
  and no for `default`. A token with 2 days left is renewed by the timer. After the anchor is
  deleted, the old token → 401.

---

## A4. K13: hostd journal PVC

```yaml
apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: ccsbx-hostd-journal, namespace: cc-sandbox-doors}
spec:
  storageClassName: local-path       # the cluster default; binds on first use -> future-two
  accessModes: [ReadWriteOnce]
  resources: {requests: {storage: 5Gi}}
```

- **Mounted only by** the `ccsbx-hostd` Deployment (1 replica, `strategy: Recreate`, so two
  pods never share it) at `/var/lib/ccsbx/journal`.
- The launcher can't mount it, because it's in a namespace the launcher has no role in, and
  `cc-sandbox` keeps its PVC quota at 0.
- **local-path ignores the size.** The real cap is in hostd: `CCSBX_JOURNAL_MAX_BYTES=4Gi`, with
  14-day retention after the cursor (doc 5 M3). Past the cap, hostd refuses new events with
  `journal_full` and alerts.
- **On the node:** the k3s local-path directory on future-two (on the LUKS root). There's no
  backup. A lost journal is an epoch reset, which hostd handles by re-ingesting
  (`T:epoch-reset-reingest`).
- **Acceptance:** after `kubectl -n cc-sandbox-doors delete pod -l cc-sandbox/role=hostd`, the
  journal head seq is unchanged after the restart (`T:journal-durable`).

---

## A5. NetworkPolicy on the operator egress proxies

The operator makes **one egress proxy per ExternalName Service**. The pods run in namespace
`tailscale` (on the `k3s` control-plane node), labelled `tailscale.com/parent-resource=<svc>` and
`tailscale.com/parent-resource-ns=<ns>`. We need three, all tagged `tag:ccsbx`:

| Egress Service (ns `cc-sandbox`) | Tailnet target | Port | Used by |
|---|---|---|---|
| `session-bus-egress` (the live draft variant E) | hp-ubuntu | 4011 | session pods (the sidecar) and hostd |
| `hp-hostd-egress` | hp-ubuntu | 7480 | hostd only (M2d direct fallback) |
| `mac-hostd-egress` | the Mac | 7480 | hostd only (M2d) |

```yaml
# in namespace tailscale (SHARED); one policy per proxy, so each admits only its own clients
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata: {name: ccsbx-egress-session-bus, namespace: tailscale}
spec:
  podSelector:
    matchLabels:
      tailscale.com/parent-resource: session-bus-egress
      tailscale.com/parent-resource-ns: cc-sandbox
  policyTypes: [Ingress]
  ingress:
    - from:
        - namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: cc-sandbox}}
          podSelector: {matchLabels: {cc-sandbox/role: session}}
        - namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: cc-sandbox-doors}}
          podSelector: {matchLabels: {cc-sandbox/role: hostd}}
      ports: [{protocol: TCP, port: 4011}]
---
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata: {name: ccsbx-egress-hostd-peers, namespace: tailscale}
spec:
  podSelector:
    matchExpressions:
      - {key: tailscale.com/parent-resource, operator: In, values: [hp-hostd-egress, mac-hostd-egress]}
      - {key: tailscale.com/parent-resource-ns, operator: In, values: [cc-sandbox]}
  policyTypes: [Ingress]
  ingress:
    - from:
        - namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: cc-sandbox-doors}}
          podSelector: {matchLabels: {cc-sandbox/role: hostd}}
      ports: [{protocol: TCP, port: 7480}]
```

- **Proxy egress isn't restricted.** It carries WireGuard to DERP and peers, which NetPol can't
  read. **The ACL limits the destination** (A7: `tag:ccsbx` reaches only hp:4011 and 7480, and
  the Mac:7480).
- Sessions have no DNS, so the launcher passes `session-bus-egress`'s proxy ClusterIP in through
  `hostAliases`, and hostd gets all three as env.
- **Policy 2 in `cc-sandbox`** additionally allows `session → tailscale/session-bus-egress
  proxy :4011`, which is the live variant E's NetPol.
- **NetPol is per pod.** The agent container can reach hp:4011 too. What stops it is B6c (a
  per-generation, sid-bound relay credential held only by the sidecar) and B3 (the hub rejects
  unsigned `fenced` claims).
- **Acceptance:**
  - from a probe `role=session` pod, `:4011` on the session-bus proxy connects, and `:7480` on
    the hp-hostd proxy times out;
  - from a pod without a label in `cc-sandbox`, both time out;
  - from `default`, both time out.

---

## A6. The Mac ops ServiceAccount (`ccsbx-ops`)

This is for the optional Mac kubectl session (doc 5 §7.3). It isn't cluster-admin. **It can't
patch Deployments.** A patch can change a Deployment's image or mounts, so a restart is a
**pod delete** instead (every Deployment here has 1 replica).

```yaml
apiVersion: v1
kind: ServiceAccount
metadata: {name: ccsbx-ops, namespace: cc-sandbox}
automountServiceAccountToken: false
---
# the same Role + RoleBinding in BOTH namespaces (cc-sandbox and cc-sandbox-doors)
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata: {name: ccsbx-ops, namespace: cc-sandbox}      # and namespace: cc-sandbox-doors
rules:
  - apiGroups: [""]
    resources: [pods, pods/log, events, services, endpoints, configmaps, resourcequotas, limitranges, persistentvolumeclaims]
    verbs: [get, list, watch]
  - apiGroups: [apps]
    resources: [deployments, replicasets]
    verbs: [get, list, watch]
  - apiGroups: [networking.k8s.io]
    resources: [networkpolicies]
    verbs: [get, list, watch]
  - apiGroups: [events.k8s.io]
    resources: [events]
    verbs: [get, list, watch]
  - apiGroups: [""]
    resources: [pods]
    verbs: [delete]                  # = restart (Deployment recreates) / kill a session pod
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata: {name: ccsbx-ops, namespace: cc-sandbox}      # and namespace: cc-sandbox-doors
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: Role, name: ccsbx-ops}
subjects: [{kind: ServiceAccount, name: ccsbx-ops, namespace: cc-sandbox}]
---
# Flux status for our Kustomization only (namespace flux-system): read + reconcile annotation
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata: {name: ccsbx-ops-flux, namespace: flux-system}
rules:
  - apiGroups: [kustomize.toolkit.fluxcd.io]
    resources: [kustomizations]
    resourceNames: [cc-sandbox]
    verbs: [get, patch]              # patch = `flux reconcile kustomization cc-sandbox` (annotation)
```

- **Not granted:**
  - secrets;
  - exec, attach and port-forward;
  - create;
  - update or patch on anything except our Kustomization;
  - impersonation (so the brief's `auth can-i --as=` line goes; the session uses
    `kubectl auth can-i --list` as itself);
  - `flux reconcile … --with-source`, which would patch the shared GitRepository.
- **Optional:** a VAP rule on DELETE, `oldObject.metadata.labels['cc-sandbox/role'] in ['session',
  'door', 'hostd']` for this SA, so it can never delete the watchdog.
- **The kubeconfig** gets a TokenRequest token as in A3: 30 days, bound to its own anchor
  `ccsbx-ops-anchor`, and SOPS-encrypted to the Mac's age key. The Mac renews it by hand; it's
  the optional session.
- **Acceptance (as `ccsbx-ops`):**
  - `can-i get secrets` → no in both namespaces;
  - `can-i create pods/exec` → no;
  - `can-i patch deployments` → no;
  - `can-i delete pods -n cc-sandbox-doors` → yes;
  - `flux reconcile kustomization cc-sandbox` works.

---

## A7. Tailnet ACL (paste-ready fragments for `artifacts/tailscale-config.json`)

**tagOwners.** The operator must own the tag, or it can't create proxy devices with it, so
`tag:k8s-operator` has to be listed. The manager's recommendation of `autogroup:admin` is kept
as well, so Tomas can tag a probe device:

```jsonc
	"tagOwners": {
		// … existing entries …
		"tag:ccsbx":         ["autogroup:admin", "tag:k8s-operator"],
	},
```

**hosts.** `hp-ubuntu` exists. Add the Mac (an untagged personal device, like hp; don't tag it,
for the same reason the file gives for hp):

```jsonc
	"hosts": {
		// … existing entries …
		"tomas-mac": "<the Mac's tailnet IP>/32",
	},
```

**grants:**

```jsonc
		// cc-sandbox: operator EGRESS proxies (tag:ccsbx) -> the session-bus hub and the session
		// daemons on hp and the Mac. Nothing else is reachable from tag:ccsbx.
		{"src": ["tag:ccsbx"], "dst": ["hp-ubuntu"], "ip": ["tcp:4011", "tcp:7480"]},
		{"src": ["tag:ccsbx"], "dst": ["tomas-mac"], "ip": ["tcp:7480"]},
		// cc-sandbox: hp and the Mac -> door and hostd INGRESS proxies (tag:ccsbx), HTTPS only.
		{"src": ["hp-ubuntu", "tomas-mac"], "dst": ["tag:ccsbx"], "ip": ["tcp:443"]},
		// cc-sandbox launcher on hp -> kube-apiserver (tag:infra). ALREADY LIVE (keep as is).
		{"src": ["hp-ubuntu"], "dst": ["tag:infra"], "ip": ["tcp:6443"]},
```

Notes:
- **hp ↔ Mac on 7480** needs no new grant. Both are tomas's devices, so the existing
  `autogroup:member → autogroup:self` grant covers them. The host firewalls are the limit: ufw
  on hp, and the macOS firewall.
- **`tag:ccsbx` is shared by the egress and the ingress proxies.** So the door ingress proxies
  could also reach hp:4011 and 7480. They're operator-managed, never agent-controlled, so this
  is accepted. If that's not good enough, split the tag into `tag:ccsbx-egress`, which carries
  the outbound grants, and `tag:ccsbx`.
- The existing `autogroup:admin → *` grant already lets Tomas's devices reach everything. The
  explicit grants above keep working once that grant is tightened, as the file plans.
- **Acceptance:**
  - `tailscale status` lists the proxy devices with `tag:ccsbx`;
  - from the session-bus proxy, hp:4011 connects and hp:22 times out;
  - from hp, a door's `https://<door device>/health` → 200.

---

## Q13. Secrets-encryption: a separate k3s analysis task

Encrypting Secrets at rest (k3s `--secrets-encryption`) is **split out as its own k3s analysis
task**. It's a server-node change on a shared cluster. **Until it's done, only test keys go in**:
- the Console-capped API key;
- the testbed-only GitHub App;
- the e2e approver key;
- the hub credential;
- the pull token.

**The subscription token (`cc-sandbox-model-sub`, doc 5 R5b) waits for it.** Until then, fenced
sessions run on the API key or the stub, and the subscription stays with unfenced sessions.

## Related

[Doc 5 build plan](cc-unified-model-build-plan.md) §4.6, §7.2 R5b, §7.5 · the k3s cooperation scope (devops:3crt) · k3s `apps/cc-sandbox/` at `8aaacc0`
