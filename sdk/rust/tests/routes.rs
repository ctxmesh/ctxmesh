//! The fake is generated from sdk/launcher-routes.json, whose goldens cmd/launcher proves against
//! the real handlers. It 404s anything the launcher does not serve and records every request that
//! breaks a route's schema, so a client calling the wrong path, or sending the wrong field, fails
//! here.

mod fake_launcher;

use ctxmesh::{Client, Config, Entry, Error};
use fake_launcher::FakeLauncher;
use std::collections::BTreeMap;

const CAP: &str = "probe-run-capability";

fn client_for(f: &FakeLauncher) -> Client {
    let p = f.port();
    Client::with_config(Config::for_test(p, p, p, p, "conv-1"))
}

fn entry(role: &str, content: &str) -> Entry {
    Entry {
        role: role.into(),
        content: content.into(),
    }
}

#[test]
fn every_launcher_route_is_exercised() {
    let fake = FakeLauncher::start();
    let c = client_for(&fake);
    let mut tags = BTreeMap::new();
    tags.insert("topic".to_string(), "preferences".to_string());

    // Every call the plane-client tier owes. One fake serves every listener, so a route sent to
    // the wrong PORT still reaches it; delegate_uses_its_own_listener covers ports.
    let calls: Vec<(&str, Result<(), Error>)> = vec![
        ("GET /memory/{conversationId}", c.memory_get(None).map(drop)),
        (
            "POST /memory/{conversationId}/append",
            c.memory_append(&entry("user", "hi"), None),
        ),
        (
            "PUT /memory/{conversationId}",
            c.memory_put(&[entry("user", "hi")], None),
        ),
        (
            "GET /memory/{conversationId}/search",
            c.memory_search("metric", None, Some(CAP)).map(drop),
        ),
        (
            "POST /memory/agent/remember",
            c.remember("The user prefers metric units.", &tags),
        ),
        (
            "POST /memory/agent/search",
            c.search_agent("units", 3, 0.0).map(drop),
        ),
        (
            "POST /knowledge/search",
            c.knowledge_search("When do deploys go out?", Some("handbook"), 3)
                .map(drop),
        ),
        ("GET /skills", c.skills().map(drop)),
        ("POST /skills/load", c.skill_load("deploy").map(drop)),
        (
            "ANY /feedback",
            c.feedback(
                "0af7651916cd43dd8448eb211c80319c",
                "helpfulness",
                0.9,
                Some("clear answer"),
            ),
        ),
        (
            "POST /amp/{targetAgent}",
            c.call_agent("peer-agent", serde_json::json!({"question": "ping"}))
                .map(drop),
        ),
        (
            "POST /a2a/{targetAgent}",
            c.call_agent_legacy("peer-agent", serde_json::json!({"question": "ping"}))
                .map(drop),
        ),
        (
            "POST /delegate",
            c.delegate(
                "researcher",
                "1",
                "call-1",
                CAP,
                "Summarise the quarterly report".into(),
            )
            .map(drop),
        ),
        (
            "POST /handoff",
            c.handoff(
                "billing",
                CAP,
                Some("The customer is asking about an invoice."),
                true,
            )
            .map(drop),
        ),
    ];
    let failed: Vec<String> = calls
        .into_iter()
        .filter_map(|(k, r)| r.err().map(|e| format!("{k}: {e}")))
        .collect();
    assert!(failed.is_empty(), "calls failed: {failed:#?}");

    for key in fake.route_keys() {
        let why = fake.waived(&key);
        if why.is_empty() {
            assert!(
                fake.is_covered(&key),
                "the launcher serves {key} and no SDK call reached it"
            );
        } else {
            assert!(
                !fake.is_covered(&key),
                "{key} is waived for Rust ({why}) but the SDK reaches it now: delete the waiver"
            );
        }
    }
    fake.assert_conforms();
}

#[test]
fn responses_decode_as_the_launcher_sends_them() {
    let fake = FakeLauncher::start();
    let c = client_for(&fake);

    let entries = c.memory_get(Some("conv-1")).expect("memory_get");
    assert_eq!(entries.len(), 2);
    assert_eq!(entries[1].content, "You prefer metric units.");
    let facts = c.search_agent("units", 3, 0.0).expect("search_agent");
    assert_eq!(facts.len(), 1);
    assert_eq!(facts[0].content, "The user prefers metric units.");
    assert_eq!(facts[0].score, 0.91);
    let chunks = c
        .knowledge_search("When do deploys go out?", Some("handbook"), 3)
        .expect("knowledge");
    assert_eq!(chunks.len(), 1);
    assert_eq!(chunks[0].document_ref, "handbook/deploys.md");
    let skills = c.skills().expect("skills");
    assert_eq!(skills.len(), 1);
    assert_eq!(skills[0].name, "deploy");
    let reply = c
        .call_agent("peer-agent", serde_json::json!({"question": "ping"}))
        .expect("call_agent");
    assert_eq!(reply["answer"], "pong");
    let h = c
        .handoff("billing", CAP, Some("over to you"), true)
        .expect("handoff");
    assert!(h.ok);
    assert_eq!(h.run_id, "run-billing-1");
    assert_eq!(h.handed_off_to, "billing");
    fake.assert_conforms();
}

#[test]
fn skill_load_returns_the_body() {
    // The launcher answers {"body": …}. Reading "content" gave "" with NO error, so a skill loaded
    // as nothing and the model carried on without it.
    let fake = FakeLauncher::start();
    let want = fake.golden("POST /skills/load", "")["response"]["body"]["body"]
        .as_str()
        .expect("the golden carries a body")
        .to_string();
    let got = client_for(&fake).skill_load("deploy").expect("load");
    assert!(!got.is_empty());
    assert_eq!(got, want);
}

#[test]
fn refusals_surface_the_launcher_status() {
    let fake = FakeLauncher::start();
    fake.use_golden(
        "POST /knowledge/search",
        "refuses-a-knowledge-base-not-granted",
    );
    fake.use_golden("POST /skills/load", "refuses-a-skill-not-attached");
    fake.use_golden("POST /memory/agent/remember", "requires-content");
    let c = client_for(&fake);

    assert!(matches!(
        c.knowledge_search("salaries", Some("payroll"), 3),
        Err(Error::Denied { .. })
    ));
    assert!(matches!(c.skill_load("payroll"), Err(Error::Denied { .. })));
    assert!(matches!(
        c.remember("  ", &BTreeMap::new()),
        Err(Error::Api { status: 400, .. })
    ));
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
    // was a 404 in all four plane-client SDKs.
    let memory = FakeLauncher::start();
    let delegate = FakeLauncher::start();
    let c = Client::with_config(Config::for_test(
        memory.port(),
        memory.port(),
        memory.port(),
        delegate.port(),
        "c",
    ));
    c.delegate("researcher", "1", "call-1", CAP, "x".into())
        .expect("delegate");
    assert!(
        delegate.is_covered("POST /delegate"),
        "/delegate did not reach the delegate listener"
    );
    assert!(
        !memory.is_covered("POST /delegate"),
        "/delegate reached the MEMORY listener"
    );
}

#[test]
fn delegate_decodes_success_and_refusal() {
    // The launcher answers 200 for every outcome and signals success in "ok", so a refusal decoded
    // as a transport success would be silent data loss, and "answer" is the point of delegating.
    let fake = FakeLauncher::start();
    let c = client_for(&fake);
    let input: serde_json::Value = "Summarise the quarterly report".into();

    let d = c
        .delegate("researcher", "1", "call-1", CAP, input.clone())
        .expect("delegate");
    assert!(d.ok, "ok must be decoded");
    assert_eq!(d.answer, "The quarterly report is ready.");
    assert_eq!(d.sub_run, "sub-run-1");

    fake.use_golden("POST /delegate", "refuses-without-a-run-capability");
    let d = c
        .delegate("researcher", "1", "call-1", CAP, input)
        .expect("delegate");
    assert!(!d.ok, "a refusal must decode as ok=false");
    assert!(!d.error.is_empty(), "a refusal carries its reason");
    fake.assert_conforms();
}

#[test]
fn the_fake_refuses_what_the_launcher_refuses() {
    // The fake is only worth generating if it catches the defects hand-written fakes let through.
    let fake = FakeLauncher::start();
    let base = format!("http://127.0.0.1:{}", fake.port());
    let _ = ureq_post(
        &format!("{base}/memory/agent/remember"),
        r#"{"text":"a fact"}"#,
    );
    assert!(
        !fake.violations().is_empty(),
        "a remember body without content passed the fake"
    );
    assert_eq!(
        ureq_post(&format!("{base}/memory/agent/forget"), "{}"),
        404,
        "a path the launcher does not serve must 404"
    );
    assert_eq!(
        ureq_post(&format!("{base}/skills"), "{}"),
        405,
        "a served path under the wrong method must 405"
    );
}

/// The status of a raw POST, outside the SDK.
fn ureq_post(url: &str, body: &str) -> u16 {
    match ureq::post(url)
        .set("Content-Type", "application/json")
        .send_string(body)
    {
        Ok(r) => r.status(),
        Err(ureq::Error::Status(code, _)) => code,
        Err(e) => panic!("POST {url}: {e}"),
    }
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
    let fake = FakeLauncher::start();
    let c = client_for(&fake);
    assert!(matches!(
        c.knowledge_search("q", None, 3),
        Err(Error::Invalid(_))
    ));
    assert!(fake.received().is_empty(), "refused locally, nothing sent");
}
