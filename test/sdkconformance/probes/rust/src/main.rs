//! Drives the Rust SDK's public API against a real launcher, one call per contract route, and
//! prints "PASS|FAIL|MISSING <route>[: reason]" for each. hack/sdk-conformance runs it and checks
//! every PASS against what the launcher actually received.

use ctxmesh::{Client, Entry, Error};
use serde_json::{json, Value};
use std::collections::BTreeMap;

type Check = fn(&Client, &str, &Value) -> Result<(), String>;

fn main() {
    let path = std::env::var("CTXMESH_PROBE_CONTRACT").unwrap_or_default();
    let raw = std::fs::read_to_string(&path).unwrap_or_else(|e| {
        eprintln!("probe: read the contract {path:?}: {e}");
        std::process::exit(2);
    });
    let contract: Value = serde_json::from_str(&raw).unwrap_or_else(|e| {
        eprintln!("probe: parse the contract: {e}");
        std::process::exit(2);
    });
    let capability = std::env::var("CTXMESH_PROBE_CAPABILITY").unwrap_or_default();
    // from_env, not an explicit Config: the env the launcher publishes is part of what is checked.
    let client = Client::from_env();
    for route in contract["routes"].as_array().into_iter().flatten() {
        let key = format!("{} {}", s(&route["method"]), s(&route["path"]));
        // The route's sdkResult rides with the golden, so expect can require those fields.
        let mut golden = route["goldens"][0].clone();
        golden["sdkResult"] = route["sdkResult"].clone();
        let golden = &golden;
        match (&client, check_for(&key)) {
            (Err(e), _) => {
                println!("FAIL {key}: the SDK does not configure from the launcher's env: {e}")
            }
            (Ok(_), None) => println!("MISSING {key}: the Rust SDK has no call for it"),
            (Ok(c), Some(check)) => match check(c, &capability, golden) {
                Ok(()) => println!("PASS {key}"),
                Err(e) => println!("FAIL {key}: {}", e.replace('\n', " ")),
            },
        }
    }
}

fn check_for(key: &str) -> Option<Check> {
    let check: Check = match key {
        "POST /a2a/{targetAgent}" => |c, _, g| {
            let reply = c.call_agent_legacy(&segment(g, 1), g["request"]["body"].clone());
            expect("reply", reply.map_err(err), g)
        },
        "POST /amp/{targetAgent}" => |c, _, g| {
            let reply = c.call_agent(&segment(g, 1), g["request"]["body"].clone());
            expect("reply", reply.map_err(err), g)
        },
        "POST /delegate" => |c, cap, g| {
            let b = &g["request"]["body"];
            let d = c.delegate(
                s(&b["subAgent"]),
                s(&b["step"]),
                s(&b["callId"]),
                cap,
                b["input"].clone(),
            );
            expect(
                "delegation",
                d.map(|d| {
                    json!({"ok": d.ok, "subAgent": d.sub_agent, "subRun": d.sub_run,
                        "answer": d.answer, "error": d.error, "suspend": d.suspend,
                        "endpoint": d.endpoint})
                })
                .map_err(err),
                g,
            )
        },
        "ANY /feedback" => |c, _, g| {
            let b = &g["request"]["body"];
            let comment = b["comment"].as_str();
            c.feedback(
                s(&b["traceId"]),
                s(&b["name"]),
                b["value"].as_f64().unwrap_or_default(),
                comment,
            )
            .map_err(err)
        },
        "POST /handoff" => |c, cap, g| {
            let b = &g["request"]["body"];
            let h = c.handoff(
                s(&b["targetAgent"]),
                cap,
                b["message"].as_str(),
                b["includeHistory"].as_bool().unwrap_or(true),
            );
            expect(
                "handoff",
                h.map(|h| {
                    json!({"ok": h.ok, "runId": h.run_id, "handedOffTo": h.handed_off_to,
                        "error": h.error})
                })
                .map_err(err),
                g,
            )
        },
        "POST /knowledge/search" => |c, _, g| {
            let b = &g["request"]["body"];
            let chunks = c.knowledge_search(
                s(&b["query"]),
                b["knowledgeBase"].as_str(),
                b["topK"].as_u64().unwrap_or_default() as usize,
            );
            expect(
                "chunks",
                chunks
                    .map(|cs| {
                        let rs: Vec<Value> = cs
                            .iter()
                            .map(|c| {
                                json!({"content": c.content, "documentRef": c.document_ref,
                                    "knowledgeBase": c.knowledge_base, "score": c.score})
                            })
                            .collect();
                        json!({ "results": rs })
                    })
                    .map_err(err),
                g,
            )
        },
        "POST /memory/agent/remember" => |c, _, g| {
            let b = &g["request"]["body"];
            let tags: BTreeMap<String, String> = b["tags"]
                .as_object()
                .into_iter()
                .flatten()
                .map(|(k, v)| (k.clone(), s(v).to_string()))
                .collect();
            c.remember(s(&b["content"]), &tags).map_err(err)
        },
        "POST /memory/agent/search" => |c, _, g| {
            let b = &g["request"]["body"];
            let facts = c.search_agent(
                s(&b["query"]),
                b["topK"].as_u64().unwrap_or_default() as usize,
                0.0,
            );
            expect(
                "facts",
                facts
                    .map(|fs| {
                        let rs: Vec<Value> = fs
                            .iter()
                            .map(|f| json!({"content": f.content, "score": f.score}))
                            .collect();
                        json!({ "results": rs })
                    })
                    .map_err(err),
                g,
            )
        },
        "GET /memory/{conversationId}" => |c, _, g| {
            let entries = c.memory_get(Some(&segment(g, 1)));
            expect("entries", entries.map(|e| json!(e)).map_err(err), g)
        },
        "PUT /memory/{conversationId}" => |c, _, g| {
            let entries: Vec<Entry> =
                serde_json::from_value(g["request"]["body"].clone()).map_err(|e| e.to_string())?;
            c.memory_put(&entries, Some(&segment(g, 1))).map_err(err)
        },
        "POST /memory/{conversationId}/append" => |c, _, g| {
            let e: Entry =
                serde_json::from_value(g["request"]["body"].clone()).map_err(|e| e.to_string())?;
            c.memory_append(&e, Some(&segment(g, 1))).map_err(err)
        },
        "GET /memory/{conversationId}/search" => |c, _, g| {
            let q = s(&g["request"]["query"]["q"]);
            let entries = c.memory_search(q, Some(&segment(g, 1)), None);
            expect("entries", entries.map(|e| json!(e)).map_err(err), g)
        },
        "GET /skills" => |c, _, g| {
            let skills = c.skills().map(|ss| {
                let rs: Vec<Value> = ss
                    .iter()
                    .map(|k| json!({"name": k.name, "description": k.description}))
                    .collect();
                json!({ "skills": rs })
            });
            expect("skills", skills.map_err(err), g)
        },
        "POST /skills/load" => |c, _, g| {
            let body = c.skill_load(s(&g["request"]["body"]["name"]));
            expect("skill", body.map(|b| json!({ "body": b })).map_err(err), g)
        },
        _ => return None,
    };
    Some(check)
}

/// Compares what the SDK decoded with the launcher's golden answer. Every field the SDK's type
/// carries must hold the launcher's value; a field the SDK does not model is not its concern.
fn expect(what: &str, got: Result<Value, String>, g: &Value) -> Result<(), String> {
    let got = got?;
    let want = &g["response"]["body"];
    if let Some(at) = within(&got, want, "$") {
        return Err(format!(
            "{what} decoded as {got}, but the launcher sent {want} (differs at {at})"
        ));
    }
    // within compares only what the SDK kept, so it passes an SDK that dropped a field.
    for path in g["sdkResult"].as_array().into_iter().flatten() {
        let path = s(path);
        let missed = if path == "$" {
            (!same(&got, want)).then(|| "$".to_string())
        } else {
            carry(&got, want, &path.split('.').collect::<Vec<_>>(), "$")
        };
        if let Some(at) = missed {
            return Err(format!(
                "{what} decoded as {got}, which does not carry {path} with the launcher's value \
                 (at {at}); the launcher sent {want}"
            ));
        }
    }
    Ok(())
}

fn same(got: &Value, want: &Value) -> bool {
    within(got, want, "$").is_none() && within(want, got, "$").is_none()
}

/// Where `got` fails to hold `want`'s value at a sdkResult path ("[]" descends into each element).
fn carry(got: &Value, want: &Value, segs: &[&str], at: &str) -> Option<String> {
    let Some((first, rest)) = segs.split_first() else {
        return (!same(got, want)).then(|| at.to_string());
    };
    let (key, each) = match first.strip_suffix("[]") {
        Some(k) => (k, true),
        None => (*first, false),
    };
    let (got, want, at) = if key.is_empty() {
        (got, want, at.to_string())
    } else {
        let Some(g) = got.get(key) else {
            return Some(format!("{at}.{key}"));
        };
        (g, &want[key], format!("{at}.{key}"))
    };
    if !each {
        return carry(got, want, rest, &at);
    }
    let (Some(ga), Some(wa)) = (got.as_array(), want.as_array()) else {
        return Some(at);
    };
    if ga.len() != wa.len() {
        return Some(at);
    }
    ga.iter()
        .zip(wa)
        .enumerate()
        .find_map(|(i, (gv, wv))| carry(gv, wv, rest, &format!("{at}[{i}]")))
}

/// Where `got` first differs from `want`, or None: every non-empty value in got appears, equal,
/// at the same place in want, and arrays have the same length.
fn within(got: &Value, want: &Value, at: &str) -> Option<String> {
    match got {
        Value::Object(gm) => {
            let Some(wm) = want.as_object() else {
                return Some(at.to_string());
            };
            for (k, v) in gm {
                if is_zero(v) && !wm.contains_key(k) {
                    continue;
                }
                if let Some(p) = within(v, wm.get(k).unwrap_or(&Value::Null), &format!("{at}.{k}"))
                {
                    return Some(p);
                }
            }
            None
        }
        Value::Array(ga) => {
            let Some(wa) = want.as_array().filter(|wa| wa.len() == ga.len()) else {
                return Some(at.to_string());
            };
            ga.iter()
                .zip(wa)
                .enumerate()
                .find_map(|(i, (gv, wv))| within(gv, wv, &format!("{at}[{i}]")))
        }
        Value::Number(gn) => match (gn.as_f64(), want.as_f64()) {
            (Some(a), Some(b)) if a == b => None,
            _ => Some(at.to_string()),
        },
        _ => (got != want).then(|| at.to_string()),
    }
}

fn is_zero(v: &Value) -> bool {
    match v {
        Value::Null => true,
        Value::String(x) => x.is_empty(),
        Value::Bool(b) => !b,
        Value::Number(n) => n.as_f64() == Some(0.0),
        Value::Object(m) => m.is_empty(),
        Value::Array(_) => false,
    }
}

fn segment(g: &Value, i: usize) -> String {
    s(&g["request"]["path"])
        .trim_matches('/')
        .split('/')
        .nth(i)
        .unwrap_or_default()
        .to_string()
}

fn s(v: &Value) -> &str {
    v.as_str().unwrap_or_default()
}

fn err(e: Error) -> String {
    e.to_string()
}
