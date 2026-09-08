//! The contract gate proves each route STRING appears in the crate, which a comment would
//! satisfy. These drive a fake launcher and assert the client actually issued the request.

use ctxmesh::{Client, Config, Entry, Error};
use std::collections::BTreeMap;
use std::sync::{Arc, Mutex};

/// Spins a fake launcher on an ephemeral port and records what it is asked.
fn plane(status: u16) -> (Client, Arc<Mutex<Vec<String>>>) {
    let server = tiny_http::Server::http("127.0.0.1:0").expect("bind");
    let port = server.server_addr().to_ip().expect("ip").port();
    let seen = Arc::new(Mutex::new(Vec::new()));
    let recorder = Arc::clone(&seen);

    std::thread::spawn(move || {
        for req in server.incoming_requests() {
            let path = req.url().split('?').next().unwrap_or("").to_string();
            recorder.lock().unwrap().push(format!("{} {}", req.method(), path));
            let body = match path.as_str() {
                "/memory/agent/search" => {
                    r#"{"results":[{"content":"strong","score":0.9},{"content":"weak","score":0.1}]}"#
                }
                "/knowledge/search" => r#"{"results":[{"content":"c","documentRef":"d","score":0.5}]}"#,
                "/skills" => r#"{"skills":[{"name":"s","description":"d"}]}"#,
                "/skills/load" => r#"{"content":"body"}"#,
                "/delegate" => r#"{"runId":"r1","accepted":true}"#,
                p if p.starts_with("/memory/") => r#"[{"role":"user","content":"hi"}]"#,
                _ => "{}",
            };
            let resp = tiny_http::Response::from_string(body)
                .with_status_code(status)
                .with_header(
                    tiny_http::Header::from_bytes(&b"Content-Type"[..], &b"application/json"[..]).unwrap(),
                );
            let _ = req.respond(resp);
        }
    });

    (Client::with_config(Config::for_test(port, port, port, "conv-1")), seen)
}

#[test]
fn every_route_is_actually_called() {
    let (c, seen) = plane(200);

    c.memory_get(None).expect("memory get");
    c.memory_append(&Entry { role: "user".into(), content: "hi".into() }, None).expect("append");
    let mut tags = BTreeMap::new();
    tags.insert("topic".to_string(), "x".to_string());
    c.remember("a fact", &tags).expect("remember");
    c.search_agent("q", 3, 0.0).expect("search agent");
    c.knowledge_search("q", Some("kb"), 3).expect("knowledge");
    c.skills().expect("skills");
    c.skill_load("s").expect("skill load");
    c.feedback("t1", "helpfulness", 1.0, Some("clear")).expect("feedback");
    c.call_agent("other", serde_json::json!({"q":1})).expect("amp");
    c.call_agent_legacy("other", serde_json::json!({"q":1})).expect("a2a");
    c.delegate("sub", serde_json::json!({"x":1})).expect("delegate");
    c.handoff("other", true).expect("handoff");

    let got = seen.lock().unwrap().clone();
    for want in [
        "/memory/", "/memory/agent", "/memory/agent/search", "/knowledge/search", "/skills",
        "/skills/load", "/feedback", "/amp/", "/a2a/", "/delegate", "/handoff",
    ] {
        assert!(
            got.iter().any(|s| s.split(' ').nth(1).is_some_and(|p| p.starts_with(want))),
            "route {want} was never actually requested; seen={got:?}"
        );
    }
}

#[test]
fn denied_is_distinguishable_from_a_transport_failure() {
    // A 403 is the platform refusing. An agent that must behave differently when the delegate
    // fence or a budget stops it needs to branch on that.
    let (c, _) = plane(403);
    match c.delegate("sub", serde_json::Value::Null) {
        Err(Error::Denied { .. }) => {}
        other => panic!("403 must surface as Error::Denied, got {other:?}"),
    }
}

#[test]
fn unwired_capabilities_refuse_locally() {
    // The port is absent because the platform did not grant the capability. Dialling it anyway
    // turns a configuration answer into a connection refused that reads like an outage.
    let cfg = Config {
        memory_port: 1,
        feedback_port: 1,
        amp_port: 1,
        agent_name: String::new(),
        conversation_id: "c".into(),
        memory_wired: false,
        feedback_wired: false,
        long_term_enabled: false,
        knowledge_enabled: false,
    };
    let c = Client::with_config(cfg);
    assert!(matches!(c.memory_get(None), Err(Error::NotWired(_))));
    assert!(matches!(c.feedback("t", "d", 1.0, None), Err(Error::NotWired(_))));
    assert!(matches!(c.knowledge_search("q", None, 1), Err(Error::NotWired(_))));
}

#[test]
fn min_score_filters_weak_facts() {
    let (c, _) = plane(200);
    let got = c.search_agent("q", 5, 0.5).expect("search");
    assert_eq!(got.len(), 1, "minScore must drop weak hits");
    assert_eq!(got[0].content, "strong");
}

#[test]
fn ambiguous_conversation_is_refused() {
    let (c, _) = plane(200);
    let cfg = Config::for_test(1, 1, 1, "");
    let bare = Client::with_config(cfg);
    assert!(matches!(bare.memory_get(None), Err(Error::Invalid(_))));
    assert!(matches!(
        c.memory_append(&Entry { role: "u".into(), content: "x".into() }, Some("has/slash")),
        Err(Error::Invalid(_))
    ));
}
