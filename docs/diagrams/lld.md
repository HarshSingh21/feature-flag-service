# LLD Diagrams — Feature Flag Service

Companion to `PLAN.md` Phase 2. Component-level detail: the evaluation pipeline,
type model, validation flow, and the never-throw boundary.

> **O1** = bucketing key, **O2** = rule-vs-rollout precedence. Both are still your
> open decisions. They appear below as **swappable stages**, not as chosen behaviour.

---

## L1 — Evaluation pipeline

Every terminal node returns a value. There is no path that returns an error to the
caller — that is the never-throw contract made structural rather than promised.

```mermaid
flowchart TD
    A["Evaluate request<br/>flag and env and context and default"] --> REC["Panic recover boundary"]
    REC --> B{"snapshot available"}
    B -->|no| DEF1["Return caller default<br/>reason ERROR"]
    B -->|yes| PIN["Pin snapshot generation"]
    PIN --> C{"flag exists in env"}
    C -->|no| DEF2["Return caller default<br/>reason FLAG NOT FOUND"]
    C -->|yes| D{"flag enabled"}
    D -->|no| OFF["Return off value<br/>reason DISABLED"]
    D -->|yes| PREC["Precedence stage<br/>O2 swappable"]
    PREC --> E{"targeting rule matches"}
    E -->|yes| RM["Return rule value<br/>reason RULE MATCH plus rule id"]
    E -->|no| F{"rollout configured"}
    F -->|no| FT["Return fallthrough value<br/>reason FALLTHROUGH"]
    F -->|yes| BK["BucketKeyStrategy<br/>O1 swappable"]
    BK --> G{"bucketing subject present"}
    G -->|no| DEF3["Return caller default<br/>reason MISSING SUBJECT"]
    G -->|yes| H["hash subject to bucket 0 to 9999"]
    H --> I{"bucket below threshold"}
    I -->|yes| RI["Return rollout on value<br/>reason ROLLOUT IN"]
    I -->|no| RO["Return rollout off value<br/>reason ROLLOUT OUT"]

    OFF --> T{"type matches declared type"}
    RM --> T
    FT --> T
    RI --> T
    RO --> T
    T -->|no| DEF4["Return caller default<br/>reason TYPE MISMATCH"]
    T -->|yes| OUT["Return value and reason and generation"]

    REC -.->|panic caught| DEF5["Return caller default<br/>reason ERROR<br/>emit structured log"]
```

---

## L2 — One evaluate call, across the boundary

```mermaid
sequenceDiagram
    autonumber
    participant APP as App handler
    participant SDK as Thin client
    participant TR as Transport
    participant EV as Evaluator
    participant SNAP as Snapshot store
    participant LOG as Structured log

    APP->>SDK: BoolValue ctx flag evalCtx default
    Note over SDK: hard timeout armed
    SDK->>TR: Evaluate rpc with trace id
    TR->>EV: evaluate request
    EV->>SNAP: pin current generation
    SNAP-->>EV: immutable snapshot N
    EV->>EV: rules then rollout per precedence stage
    EV-->>TR: value and reason and generation
    TR-->>SDK: response
    SDK-->>APP: typed value

    alt internal error or panic
        EV->>LOG: structured error with flag and env and trace id
        EV-->>TR: flag default and reason ERROR
    end
    alt service unreachable or timeout
        SDK->>LOG: client side degraded event
        SDK-->>APP: caller default and reason ERROR
    end
```

---

## L3 — Core type model

```mermaid
classDiagram
    class Flag {
        +string Name
        +ValueType Type
        +Value DefaultValue
        +bool Enabled
        +Rule[] Rules
        +Rollout Rollout
        +string BucketSalt
    }
    class Rule {
        +string ID
        +Condition[] Conditions
        +LogicOp Combiner
        +Value Value
    }
    class Condition {
        +string Attribute
        +Operator Op
        +Value[] Values
    }
    class Rollout {
        +int Percentage
        +Value OnValue
        +Value OffValue
        +string BucketBy
    }
    class Snapshot {
        +int64 Generation
        +string Env
        +FlagMap Flags
        +Get(name) Flag
    }
    class EvalContext {
        +string UserID
        +string TenantID
        +AttrMap Attributes
    }
    class Result {
        +Value Value
        +Reason Reason
        +string RuleID
        +int64 Generation
    }
    class Evaluator {
        +Evaluate(req) Result
    }
    class BucketKeyStrategy {
        <<interface>>
        +Key(flag, ctx) string
    }
    class PrecedenceStrategy {
        <<interface>>
        +Order() Stage[]
    }

    Flag "1" o-- "many" Rule
    Rule "1" o-- "many" Condition
    Flag "1" o-- "zero or one" Rollout
    Snapshot "1" o-- "many" Flag
    Evaluator ..> Snapshot
    Evaluator ..> EvalContext
    Evaluator ..> BucketKeyStrategy
    Evaluator ..> PrecedenceStrategy
    Evaluator ..> Result
```

`BucketKeyStrategy` is the single plug point for **O1**. `PrecedenceStrategy` is the
single plug point for **O2**. Choosing either later touches one file, not the pipeline.

---

## L4 — Config validation, pre-merge and post-merge

A layer that is valid **alone** can merge into a flag that is invalid. Both passes are
needed. Nothing invalid ever replaces a serving snapshot.

```mermaid
flowchart TD
    IN["incoming layer"] --> P1{"schema parse ok"}
    P1 -->|no| R1["REJECT<br/>malformed layer"]
    P1 -->|yes| P2{"pre merge checks"}
    P2 -->|fail| R2["REJECT<br/>percentage outside 0 to 100<br/>unknown value type<br/>duplicate rule id"]
    P2 -->|pass| MERGE["deep merge onto base"]
    MERGE --> P3{"post merge checks"}
    P3 -->|fail| R3["REJECT<br/>rule value type mismatch with flag type<br/>overlay flag with no base entry<br/>rollout without on and off values"]
    P3 -->|pass| BUILD["build immutable snapshot"]
    BUILD --> SWAP["atomic pointer swap<br/>generation increments"]
    R1 --> LKG["last known good keeps serving<br/>emit config apply failure metric"]
    R2 --> LKG
    R3 --> LKG
```

---

## L5 — Sticky bucketing mechanism

Stickiness holds because the hash is pure, stable across restarts, and depends on
nothing but the key. Changing the hash or the key **re-buckets every user mid-rollout**
— treat it as a change-controlled operation, not a refactor.

```mermaid
flowchart LR
    CTX["EvalContext"] --> KS["BucketKeyStrategy<br/>O1 - not yet chosen"]
    FLG["Flag name and BucketSalt"] --> KS
    KS --> KEY["bucket key string"]
    KEY --> HASH["stable 64 bit hash<br/>non cryptographic"]
    HASH --> MOD["modulo 10000"]
    MOD --> BKT["bucket 0 to 9999"]
    BKT --> CMP{"bucket below percentage times 100"}
    CMP -->|yes| ON["rollout on value"]
    CMP -->|no| OFFV["rollout off value"]
```

Bucket space is 0–9999 rather than 0–99 so that fractional percentages such as 0.5%
are expressible without a later breaking change to the bucketing maths.

---

## L6 — The never-throw boundary

```mermaid
flowchart TD
    subgraph exported["Every exported entry point"]
        E1["BoolValue"]
        E2["StringValue"]
        E3["IntValue"]
        E4["EvaluateBatch"]
    end
    E1 --> G["defer recover<br/>on the calling goroutine"]
    E2 --> G
    E3 --> G
    E4 --> G
    G --> CORE["core pipeline"]
    CORE -->|normal| OK["typed value plus reason"]
    CORE -.->|panic| CAUGHT["recover"]
    CAUGHT --> LOGV["emit structured error<br/>flag env trace id generation stack"]
    LOGV --> MET["increment default fallback metric"]
    MET --> DV["return caller supplied default"]
```

**The trap:** `recover` only catches panics on its own goroutine. Any goroutine the
engine spawns — a config watcher, a batch fan-out — needs its own boundary. A panic in
an unguarded background goroutine kills the whole process, taking every flag with it.

---

## Evaluation reason enum

| Reason | Fires when | Serves |
|---|---|---|
| `RULE_MATCH` | a targeting rule matched | rule value, rule id attached |
| `ROLLOUT_IN` | bucket below threshold | rollout on value |
| `ROLLOUT_OUT` | bucket at or above threshold | rollout off value |
| `FALLTHROUGH` | no rule matched, no rollout | flag fallthrough value |
| `DISABLED` | flag exists but is off | flag off value |
| `FLAG_NOT_FOUND` | no such flag in this env | caller default |
| `TYPE_MISMATCH` | resolved value type is not the declared type | caller default |
| `MISSING_SUBJECT` | rollout configured, bucketing subject absent | caller default |
| `ERROR` | internal fault or recovered panic | caller default |

Every non-error reason is a **normal** outcome. Only `TYPE_MISMATCH`, `ERROR`, and a
rising `FLAG_NOT_FOUND` rate are worth alerting on — the rest are the system working.
