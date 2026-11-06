# zrelay specifications

| Document | Scope |
|---|---|
| [contract.md](contract.md) | The **Shared Contract**: keys, the `zrec` encrypted clip format, the sidecar schema and the storage layout. |
| [relay-spec.md](relay-spec.md) | The **`zrelay`** recorder. |
| [ui-spec.md](ui-spec.md) | The **`zview`** web viewer and **`zview-server`**. |
| [../superpowers/specs/2026-11-06-zrelay-v2-design.md](../superpowers/specs/2026-11-06-zrelay-v2-design.md) | The **v2 rewrite design**: amendments to the three documents above, new scope (live view, RTSP restream), deployment and build phases. |

## Precedence

These three documents were carried over unchanged from the v1 repository (`Draft v2.2` / `v2.1`). The v2 design document amends them. **Where they conflict, the v2 design document wins.** Its §11 lists every amendment by section.

`contract.md` remains the single source of truth for formats, keys, the sidecar schema and the storage layout. The v2 design does not change any byte format or schema field.

- Normative language is **MUST / SHOULD / MAY** per RFC 2119.
- Read `contract.md` first, then the v2 design, then whichever component spec you are building.
