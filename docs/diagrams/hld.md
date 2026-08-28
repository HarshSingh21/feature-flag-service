# HLD Diagrams — Feature Flag Service

Companion to `PLAN.md` Phase 1. All diagrams are Mermaid and render in GitHub,
GitLab, VS Code, and IntelliJ without a plugin.

---

## H1 — Container view

Who runs where, and what crosses the wire. Evaluation happens **only** inside the
Flag Evaluation Service; the client tier never sees flag config.

```mermaid
flowchart TB
    subgraph edge["Client tier - never evaluates"]
        BR["Browser"]
        MOB["Mobile app"]
    end

    subgraph app["Application backend - Go process"]
        API["Business API handlers"]
        SDK["Thin flag client<br/>pkg client"]
        CACHE["Local snapshot cache"]
    end

    subgraph ffs["Flag Evaluation Service - Go process"]
        TR["Transport layer<br/>gRPC and HTTP"]
        EV["Evaluation engine"]
        SNAP["Resolved snapshot store<br/>atomic pointer"]
        RES["Layer resolver<br/>base plus overlay merge"]
        VAL["Config validator"]
        ADM["Admin ingress<br/>set flag config"]
    end

    OP["Operator or CI pipeline"]

    BR -->|business request| API
    MOB -->|business request| API
    API -->|BoolValue StringValue IntValue| SDK
    SDK <-->|evaluate rpc| TR
    SDK --- CACHE
    TR --> EV
    EV -->|pinned read| SNAP
    OP -->|set layer| ADM
    ADM --> VAL
    VAL -->|valid| RES
    VAL -->|invalid - reject| OP
    RES -->|publish generation| SNAP
    SNAP -.->|push update under 5s| SDK
```

**The trade-off this shape buys.** A network hop now sits in the caller's hot path.
The mitigations are the local snapshot cache, a hard client timeout, and a fail-open
posture that returns the caller-supplied default rather than an error.

---

## H2 — Component view and dependency direction

Imports point **inward**. `internal/core` imports no transport, no config source,
and no logging implementation — only interfaces. That is what keeps the evaluation
engine unit-testable without a server.

```mermaid
flowchart TD
    subgraph cmd["cmd"]
        MAIN["flag service main<br/>wiring and lifecycle"]
    end

    subgraph transport["internal transport"]
        HTTP["http handler"]
        GRPC["grpc server"]
        HEALTH["health ready live"]
    end

    subgraph core["internal core - imports nothing outward"]
        EVAL["evaluator<br/>pipeline orchestration"]
        RULES["matcher<br/>targeting rules"]
        BUCKET["bucketer<br/>hash and rollout"]
        TYPES["types<br/>flag value reason"]
    end

    subgraph config["internal config"]
        LAYER["layer<br/>base and overlay model"]
        MERGE["resolver<br/>deep merge"]
        VALID["validator"]
        STORE["snapshot store<br/>atomic pointer"]
    end

    subgraph obs["internal obs"]
        LOG["structured logger"]
        MET["metrics"]
        TRACE["trace propagation"]
    end

    PKG["pkg client<br/>public thin client"]

    MAIN --> HTTP
    MAIN --> GRPC
    MAIN --> HEALTH
    MAIN --> STORE
    HTTP --> EVAL
    GRPC --> EVAL
    EVAL --> RULES
    EVAL --> BUCKET
    EVAL --> TYPES
    EVAL --> STORE
    LAYER --> MERGE
    MERGE --> VALID
    VALID --> STORE
    EVAL -.-> LOG
    EVAL -.-> MET
    HTTP -.-> TRACE
    PKG -.->|network| GRPC
```

---

## H3 — Helm-style config layering

One base layer of shared flag definitions, one overlay per environment, merged into
an immutable snapshot per environment. **The merge runs at config-write time, not per
request** — evaluation reads a prebuilt map.

```mermaid
flowchart LR
    L0["Layer 0<br/>compiled in code default"]
    L1["Layer 1<br/>base flag config"]
    L2D["Layer 2<br/>dev overlay"]
    L2S["Layer 2<br/>staging overlay"]
    L2P["Layer 2<br/>prod overlay"]

    MD["Merge for dev"]
    MS["Merge for staging"]
    MP["Merge for prod"]

    SD["Snapshot dev<br/>immutable generation N"]
    SS["Snapshot staging<br/>immutable generation N"]
    SP["Snapshot prod<br/>immutable generation N"]

    L0 --> MD
    L0 --> MS
    L0 --> MP
    L1 --> MD
    L1 --> MS
    L1 --> MP
    L2D --> MD
    L2S --> MS
    L2P --> MP
    MD --> SD
    MS --> SS
    MP --> SP
```

Precedence rises left to right. Scalars override. The list-merge rule for **targeting
rules** is the one genuinely hard call and is settled in PLAN.md Phase 1.2 — ordered
lists do not deep-merge safely.

---

## H4 — Environment isolation

A bad prod overlay cannot reach dev. Merge and rebuild are scoped to one environment.

```mermaid
flowchart TD
    IN["set overlay for prod"] --> V{"valid after merge"}
    V -->|no| REJ["REJECT<br/>prod snapshot unchanged"]
    V -->|yes| BUILD["rebuild prod snapshot only"]
    BUILD --> P["prod generation increments"]
    BUILD --> D["dev snapshot untouched"]
    BUILD --> S["staging snapshot untouched"]
    REJ --> D
    REJ --> S
```

---

## H5 — Live update propagation, end to end

The hard requirement is **under 5 seconds, no restart**. The risky hop is the last one.

```mermaid
sequenceDiagram
    autonumber
    participant OP as Operator
    participant ADM as Admin ingress
    participant VAL as Validator
    participant RES as Resolver
    participant SNAP as Snapshot store
    participant SDK as Thin client
    participant APP as App backend

    OP->>ADM: set base or overlay layer
    ADM->>VAL: validate layer
    VAL-->>OP: reject on error - last known good keeps serving
    VAL->>RES: valid layer
    RES->>RES: deep merge and build snapshot
    RES->>SNAP: atomic swap to generation N plus 1
    Note over SNAP: evaluations in flight stay pinned to generation N
    SNAP-->>SDK: push change event
    SDK->>SDK: refresh cached snapshot
    APP->>SDK: BoolValue flag and context and default
    SDK-->>APP: value and reason and generation
```

---

## H6 — Snapshot lifecycle under concurrent readers

Readers never block and never tear. An in-flight evaluation pins its snapshot once at
entry and finishes against that generation even if a swap lands mid-request.

```mermaid
stateDiagram-v2
    [*] --> Empty
    Empty --> ServingG1: first valid config applied
    ServingG1 --> Building: new layer accepted
    Building --> ServingG2: atomic pointer swap
    Building --> ServingG1: validation failed - keep last known good
    ServingG2 --> Building: next change
    ServingG2 --> [*]: shutdown
    note right of Building
        readers stay pinned to the
        previous generation until
        they complete
    end note
```
