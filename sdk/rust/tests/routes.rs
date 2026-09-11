//! Coverage is asserted against sdk/launcher-routes.json — generated from cmd/launcher — with a
//! fake that 404s anything unregistered.
//!
//! The previous fake was a permissive catch-all written by the author of the client it tests, so
//! both encoded the same wrong assumption and every test passed while 8 of 10 routes failed
//! against a real launcher.

use ctxmesh::{Client, Config, Entry, Error};
use std::collections::BTreeMap;
use std::sync::{Arc, Mutex};

const CAP: &str = "cap-token";

#[derive(Clone, Debug)]
struct Route {
    method: String,
    path: String,
}

impl Route {
    /// `{param}` matches exactly ONE segment: `/memory/{id}` must NOT match
    /// `/memory/agent/remember`. Conflating those produced the `/memory/agent` fiction.
    fn matches(&self, m: &str, p: &str) -> bool {
        if self.method != "ANY" && self.method != m {
            return false;
        }
        let want: Vec<&str> = self.path.trim_matches('/').split('/').collect();
        let got: Vec<&str> = p.trim_matches('/').split('/').collect();
        want.len() == got.len()
            && want
                .iter()
                .zip(&got)
                .all(|(w, g)| w.starts_with('{') && !g.is_empty() || w == g)
    }
}

fn load_routes() -> Vec<Route> {
    let mut dir = std::env::current_dir().expect("cwd");
    for _ in 0..6 {
        let f = dir.join("launcher-routes.json");
        if let Ok(s) = std::fs::read_to_string(&f) {
            let v: serde_json::Value = serde_json::from_str(&s).expect("fixture is not JSON");
            let out: Vec<Route> = v["routes"]
                .as_array()
                .expect("routes")
                .iter()
                .map(|r| Route {
                    method: r["method"].as_str().unwrap_or_default().to_string(),
                    path: r["path"].as_str().unwrap_or_default().to_string(),
                })
                .collect();
            assert!(
                !out.is_empty(),
                "the fixture is empty; this test would assert nothing"
            );
            return out;
        }
        if !dir.pop() {
            break;
        }
    }
    panic!("launcher-routes.json not found — the fixture IS the contract");
}

/// Mirrors what the real handlers return — verified against cmd/launcher, not against the client.
fn body_for(pattern: &str) -> &'static str {
    match pattern {
        "/memory/agent/search" => {
            r#"{"results":[{"content":"strong","score":0.9},{"content":"weak","score":0.1}]}"#
        }
        "/knowledge/search" => r#"{"results":[{"content":"c","documentRef":"d","score":0.5}]}"#,
        "/skills" => r#"{"skills":[{"name":"s","description":"d"}]}"#,
        "/skills/load" => r#"{"body":"the skill body"}"#, // "body", not "content"
        "/delegate" => r#"{"ok":true,"subAgent":"sub","subRun":"r1","answer":"42"}"#,
        "/handoff" => r#"{"ok":true,"runId":"r1","handedOffTo":"other"}"#,
        "/memory/{conversationId}" | "/memory/{conversationId}/search" => {
            r#"[{"role":"user","content":"hi"}]"#
        }
        _ => "{}",
    }
}

struct Plane {
    port: u16,
    seen: Arc<Mutex<Vec<String>>>,
    reqs: Arc<Mutex<Vec<String>>>,
}

fn plane(routes: Vec<Route>, status: u16) -> Plane {
    let server = tiny_http::Server::http("127.0.0.1:0").expect("bind");
    let port = server.server_addr().to_ip().expect("ip").port();
    let seen = Arc::new(Mutex::new(Vec::new()));
    let reqs = Arc::new(Mutex::new(Vec::new()));
    let (s, q) = (Arc::clone(&seen), Arc::clone(&reqs));

    std::thread::spawn(move || {
        for req in server.incoming_requests() {
            let path = req.url().split('?').next().unwrap_or("").to_string();
            let method = req.method().to_string();
            q.lock().unwrap().push(format!("{method} {path}"));
            match routes.iter().find(|r| r.matches(&method, &path)) {
                None => {
                    // Exactly what a real launcher does, and the whole point of this fake.
                    let _ = req.respond(
                        tiny_http::Response::from_string("404 page not found")
                            .with_status_code(404),
                    );
                }
                Some(hit) => {
                    s.lock()
                        .unwrap()
                        .push(format!("{} {}", hit.method, hit.path));
                    let _ = req.respond(
                        tiny_http::Response::from_string(body_for(&hit.path))
                            .with_status_code(status)
                            .with_header(
                                tiny_http::Header::from_bytes(
                                    &b"Content-Type"[..],
                                    &b"application/json"[..],
                                )
                                .unwrap(),
                            ),
                    );
                }
            }
        }
    });
    Plane { port, seen, reqs }
}

#[test]
fn every_launcher_route_is_exercised() {
    let routes = load_routes();
    let p = plane(routes.clone(), 200);
    let c = Client::with_config(Config::for_test(p.port, p.port, p.port, p.port, "conv-1"));

    c.memory_get(None).expect("memory_get");
    c.memory_append(
        &Entry {
            role: "user".into(),
            content: "hi".into(),
        },
        None,
    )
    .expect("append");
    c.memory_put(
        &[Entry {
            role: "user".into(),
            content: "hi".into(),
        }],
        None,
    )
    .expect("put");
    c.memory_search("hi", None, Some(CAP))
        .expect("memory_search");
    let mut tags = BTreeMap::new();
    tags.insert("topic".to_string(), "x".to_string());
    c.remember("a fact", &tags).expect("remember");
    c.search_agent("q", 3, 0.0).expect("search_agent");
    c.knowledge_search("q", Some("kb"), 3).expect("knowledge");
    c.skills().expect("skills");
    c.skill_load("s").expect("skill_load");
    c.feedback("t1", "helpfulness", 1.0, Some("clear"))
        .expect("feedback");
    c.call_agent("other", serde_json::json!({"q":1}))
        .expect("amp");
    c.call_agent_legacy("other", serde_json::json!({"q":1}))
        .expect("a2a");
    c.delegate("sub", "step-1", "call-1", CAP, serde_json::json!({"x":1}))
        .expect("delegate");
    c.handoff("other", CAP, Some("over to you"), true)
        .expect("handoff");

    let seen = p.seen.lock().unwrap().clone();
    let reqs = p.reqs.lock().unwrap().clone();
    for r in &routes {
        assert!(
            seen.contains(&format!("{} {}", r.method, r.path)),
            "launcher serves {} {} and no SDK call reached it; requests={reqs:?}",
            r.method,
            r.path
        );
    }
}

#[test]
fn skill_load_returns_the_body() {
    // The launcher answers {"body": …}. Reading "content" gave "" with NO error, so a skill loaded
    // as nothing and the model carried on without it.
    let p = plane(load_routes(), 200);
    let c = Client::with_config(Config::for_test(p.port, p.port, p.port, p.port, "c"));
    assert_eq!(c.skill_load("s").expect("load"), "the skill body");
}

#[test]
fn delegate_refuses_without_its_required_fields() {
    let c = Client::with_config(Config::for_test(1, 1, 1, 1, "c"));
    let n = serde_json::Value::Null;
    assert!(matches!(
        c.delegate("sub", "", "call", CAP, n.clone()),
        Err(Error::Invalid(_))
    ));
    assert!(matches!(
        c.delegate("sub", "step", "", CAP, n.clone()),
        Err(Error::Invalid(_))
    ));
    assert!(matches!(
        c.delegate("sub", "step", "call", "", n.clone()),
        Err(Error::NotWired(_))
    ));
    assert!(matches!(
        c.handoff("other", "", None, true),
        Err(Error::NotWired(_))
    ));
}

#[test]
fn delegate_uses_its_own_listener() {
    // /delegate and /handoff are served by a DIFFERENT listener. Sending them to the memory port
    // was a 404 in all four SDKs.
    let memory = plane(load_routes(), 200);
    let delegate = plane(load_routes(), 200);
    let c = Client::with_config(Config::for_test(
        memory.port,
        memory.port,
        memory.port,
        delegate.port,
        "c",
    ));
    c.delegate("sub", "s", "c", CAP, serde_json::Value::Null)
        .expect("delegate");

    assert!(
        delegate
            .seen
            .lock()
            .unwrap()
            .iter()
            .any(|s| s == "POST /delegate"),
        "/delegate did not reach the delegate listener"
    );
    assert!(
        !memory
            .seen
            .lock()
            .unwrap()
            .iter()
            .any(|s| s == "POST /delegate"),
        "/delegate reached the MEMORY listener — the 404 bug is back"
    );
}

#[test]
fn delegate_refusal_is_not_success() {
    // The launcher answers 200 for every outcome and signals success in "ok". Decoding
    // {runId, accepted} made refusal, failure and success indistinguishable — and dropped
    // "answer", which is the entire point of delegating.
    let p = plane(load_routes(), 200);
    let c = Client::with_config(Config::for_test(p.port, p.port, p.port, p.port, "c"));
    let d = c
        .delegate("sub", "s", "c", CAP, serde_json::Value::Null)
        .expect("delegate");
    assert!(d.ok, "ok must be decoded");
    assert_eq!(d.answer, "42", "answer must survive");
}

#[test]
fn unwired_capabilities_refuse_locally() {
    let cfg = Config {
        memory_port: 1,
        feedback_port: 1,
        amp_port: 1,
        delegate_port: 1,
        agent_name: String::new(),
        conversation_id: "c".into(),
        memory_wired: false,
        feedback_wired: false,
        long_term_enabled: false,
        knowledge_enabled: false,
    };
    let c = Client::with_config(cfg);
    assert!(matches!(c.memory_get(None), Err(Error::NotWired(_))));
    assert!(matches!(
        c.feedback("t", "d", 1.0, None),
        Err(Error::NotWired(_))
    ));
    assert!(matches!(
        c.knowledge_search("q", Some("kb"), 1),
        Err(Error::NotWired(_))
    ));
}

#[test]
fn knowledge_base_is_required() {
    // The handler answers 400 "knowledgeBase is required", so the omit-to-search-all mode the SDK
    // used to document does not exist.
    let p = plane(load_routes(), 200);
    let c = Client::with_config(Config::for_test(p.port, p.port, p.port, p.port, "c"));
    assert!(matches!(
        c.knowledge_search("q", None, 3),
        Err(Error::Invalid(_))
    ));
}
