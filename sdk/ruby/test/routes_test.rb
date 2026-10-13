# frozen_string_literal: true

# The fake is generated from sdk/launcher-routes.json, whose goldens cmd/launcher proves against the
# real handlers. It 404s anything the launcher does not serve and records every request that breaks
# a route's schema, so a client calling the wrong path, or sending the wrong field, fails here.

require "minitest/autorun"
require "json"
require_relative "../lib/ctxmesh"
require_relative "generated_fake_launcher"

class RoutesTest < Minitest::Test
  CAP = "probe-run-capability"

  def setup
    @fake = FakeLauncher.new
    @client = client_for(@fake)
  end

  def teardown = @fake.shutdown

  def client_for(fake, delegate_port: fake.port)
    p = fake.port
    Ctxmesh::Client.from_config(
      Ctxmesh::Config.new(memory_port: p, feedback_port: p, amp_port: p, delegate_port: delegate_port,
                          conversation_id: "conv-1")
    )
  end

  def test_every_launcher_route_is_exercised
    # Every call the plane-client tier owes. One fake serves every listener, so a route sent to the
    # wrong PORT still reaches it; test_delegate_uses_its_own_listener covers ports.
    calls = {
      "GET /memory/{conversationId}" => -> { @client.memory_get },
      "POST /memory/{conversationId}/append" => -> { @client.memory_append(role: "user", content: "hi") },
      "PUT /memory/{conversationId}" => -> { @client.memory_put([{ "role" => "user", "content" => "hi" }]) },
      "GET /memory/{conversationId}/search" => -> { @client.memory_search("metric", capability: CAP) },
      "POST /memory/agent/remember" => lambda {
        @client.remember("The user prefers metric units.", { "topic" => "preferences" })
      },
      "POST /memory/agent/search" => -> { @client.search_agent("units", top_k: 3) },
      "POST /knowledge/search" => lambda {
        @client.knowledge_search("When do deploys go out?", knowledge_base: "handbook", top_k: 3)
      },
      "GET /skills" => -> { @client.skills },
      "POST /skills/load" => -> { @client.skill_load("deploy") },
      "ANY /feedback" => lambda {
        @client.feedback("0af7651916cd43dd8448eb211c80319c", "helpfulness", 0.9, "clear answer")
      },
      "POST /amp/{targetAgent}" => -> { @client.call_agent("peer-agent", { "question" => "ping" }) },
      "POST /a2a/{targetAgent}" => -> { @client.call_agent_legacy("peer-agent", { "question" => "ping" }) },
      "POST /delegate" => lambda {
        @client.delegate("researcher", step: "1", call_id: "call-1", capability: CAP,
                                       input: "Summarise the quarterly report")
      },
      "POST /handoff" => lambda {
        @client.handoff("billing", capability: CAP, message: "The customer is asking about an invoice.")
      }
    }
    calls.each do |key, call|
      call.call
    rescue Ctxmesh::Error => e
      flunk "#{key}: #{e.message}"
    end

    @fake.route_keys.each do |key|
      why = @fake.waived(key)
      if why
        refute @fake.covered?(key), "#{key} is waived for Ruby (#{why}) but the SDK reaches it now: delete the waiver"
      else
        assert @fake.covered?(key), "the launcher serves #{key} and no SDK call reached it"
      end
    end
    @fake.assert_conforms(self)
  end

  def test_responses_decode_as_the_launcher_sends_them
    entries = @client.memory_get("conv-1")
    assert_equal @fake.golden("GET /memory/{conversationId}")["response"]["body"], entries

    facts = @client.search_agent("units", top_k: 3)
    assert_equal @fake.golden("POST /memory/agent/search")["response"]["body"]["results"], facts

    chunks = @client.knowledge_search("When do deploys go out?", knowledge_base: "handbook", top_k: 3)
    assert_equal "handbook/deploys.md", chunks.first["documentRef"]

    assert_equal @fake.golden("GET /skills")["response"]["body"]["skills"], @client.skills

    reply = @client.call_agent("peer-agent", { "question" => "ping" })
    assert_equal "pong", reply["answer"]

    h = @client.handoff("billing", capability: CAP, message: "over to you")
    assert_equal true, h["ok"]
    assert_equal "run-billing-1", h["runId"]
    assert_equal "billing", h["handedOffTo"]
    @fake.assert_conforms(self)
  end

  def test_skill_load_returns_the_body
    # The launcher answers {"body": ...}. Reading "content" gave "" with NO error, so a skill
    # loaded as nothing and the model carried on without it.
    want = @fake.golden("POST /skills/load")["response"]["body"]["body"]
    got = @client.skill_load("deploy")
    refute_empty got
    assert_equal want, got
  end

  def test_refusals_surface_the_launcher_status
    @fake.use_golden("POST /knowledge/search", "refuses-a-knowledge-base-not-granted")
    @fake.use_golden("POST /skills/load", "refuses-a-skill-not-attached")

    e = assert_raises(Ctxmesh::DeniedError) { @client.knowledge_search("salaries", knowledge_base: "payroll") }
    assert_equal 403, e.status
    e = assert_raises(Ctxmesh::DeniedError) { @client.skill_load("payroll") }
    assert_equal 403, e.status
    assert_match(/not attached/, e.body)
  end

  def test_delegate_refuses_without_its_required_fields
    assert_raises(Ctxmesh::Error) { @client.delegate("sub", step: "", call_id: "c", capability: CAP) }
    assert_raises(Ctxmesh::Error) { @client.delegate("sub", step: "s", call_id: "", capability: CAP) }
    assert_raises(Ctxmesh::NotWiredError) { @client.delegate("sub", step: "s", call_id: "c", capability: "") }
    assert_raises(Ctxmesh::NotWiredError) { @client.handoff("other", capability: "") }
  end

  def test_delegate_uses_its_own_listener
    # /delegate and /handoff are served by a DIFFERENT listener. Sending them to the memory port
    # was a 404 in all four plane-client SDKs.
    delegate = FakeLauncher.new
    c = client_for(@fake, delegate_port: delegate.port)
    c.delegate("researcher", step: "1", call_id: "call-1", capability: CAP, input: "x")
    assert delegate.covered?("POST /delegate"), "/delegate did not reach the delegate listener"
    refute @fake.covered?("POST /delegate"), "/delegate reached the MEMORY listener"
  ensure
    delegate&.shutdown
  end

  def test_delegate_decodes_success_and_refusal
    # The launcher answers 200 for every outcome and signals success in "ok", so a refusal decoded
    # as a transport success would be silent data loss, and "answer" is the point of delegating.
    d = @client.delegate("researcher", step: "1", call_id: "call-1", capability: CAP,
                                       input: "Summarise the quarterly report")
    assert_equal true, d["ok"]
    assert_equal "The quarterly report is ready.", d["answer"]
    assert_equal "sub-run-1", d["subRun"]

    @fake.use_golden("POST /delegate", "refuses-without-a-run-capability")
    d = @client.delegate("researcher", step: "1", call_id: "call-1", capability: CAP, input: "x")
    assert_equal false, d["ok"]
    refute_empty d["error"].to_s
    @fake.assert_conforms(self)
  end

  def test_handoff_defaults_to_including_history
    # The launcher treats an ABSENT includeHistory as TRUE. Defaulting to false handed the
    # receiver nothing -- and every other SDK defaults true.
    h = @client.handoff("billing", capability: CAP)
    assert_equal true, h["ok"]
    sent = JSON.parse(@fake.received.last[:body])
    assert_equal true, sent["includeHistory"]
  end

  def test_unwired_capabilities_refuse_locally
    bare = Ctxmesh::Client.from_config(
      Ctxmesh::Config.new(memory_port: 1, feedback_port: 1, amp_port: 1, delegate_port: 1,
                          conversation_id: "c", memory_wired: false, feedback_wired: false,
                          long_term_enabled: false, knowledge_enabled: false)
    )
    assert_raises(Ctxmesh::NotWiredError) { bare.memory_get }
    assert_raises(Ctxmesh::NotWiredError) { bare.feedback("t", "d", 1.0) }
    assert_raises(Ctxmesh::NotWiredError) { bare.knowledge_search("q", knowledge_base: "kb") }
  end

  def test_knowledge_base_is_required
    assert_raises(Ctxmesh::Error) { @client.knowledge_search("q") }
  end

  def test_config_reads_the_env_the_launcher_publishes
    cfg = Ctxmesh::Config.from_env({ "AGENT_NAME" => "a", "A2A_PORT" => "3997",
                                     "DELEGATE_PORT" => "3994" })
    assert_equal 3997, cfg.amp_port
    assert_equal 3994, cfg.delegate_port
    refute cfg.memory_wired?, "MEMORY_PORT unset must mean memory is NOT wired"
  end

  def test_the_fake_refuses_what_the_launcher_refuses
    # The fake is only worth generating if it catches the defects hand-written fakes let through.
    base = @client.config.memory_base
    @client.send(:request, "POST", "#{base}/memory/agent/remember", { text: "a fact" })
    refute_empty @fake.violations, "a remember body without content passed the fake"

    e = assert_raises(Ctxmesh::ApiError) { @client.send(:request, "POST", "#{base}/memory/agent/forget", {}) }
    assert_equal 404, e.status
    e = assert_raises(Ctxmesh::ApiError) { @client.send(:request, "GET", "#{base}/skills/load") }
    assert_equal 405, e.status
  end

  def test_a_custom_answer_must_be_one_the_launcher_can_give
    assert_raises(ArgumentError) { @fake.respond("POST /skills/load", 418, "") }
    assert_raises(ArgumentError) { @fake.respond("POST /skills/load", 200, '{"content":"x"}') }
    @fake.respond("POST /skills/load", 200, '{"body":"custom"}')
    assert_equal "custom", @client.skill_load("deploy")
  end
end
