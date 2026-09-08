# frozen_string_literal: true

# The contract gate proves each route STRING appears in the gem, which a comment would satisfy.
# These drive a fake launcher and assert the client actually issued the request.

require "minitest/autorun"
require "webrick"
require_relative "../lib/ctxmesh"

class ClientTest < Minitest::Test
  def setup
    @seen = []
    @status = 200
    @server = WEBrick::HTTPServer.new(
      Port: 0, Logger: WEBrick::Log.new(File::NULL), AccessLog: []
    )
    @server.mount_proc "/" do |req, res|
      @seen << "#{req.request_method} #{req.path}"
      res.status = @status
      res["Content-Type"] = "application/json"
      res.body = case req.path
                 when "/memory/agent/search"
                   '{"results":[{"content":"strong","score":0.9},{"content":"weak","score":0.1}]}'
                 when "/knowledge/search"
                   '{"results":[{"content":"c","documentRef":"d","score":0.5}]}'
                 when "/skills" then '{"skills":[{"name":"s","description":"d"}]}'
                 when "/skills/load" then '{"content":"body"}'
                 when "/delegate" then '{"runId":"r1","accepted":true}'
                 else
                   req.request_method == "GET" && req.path.start_with?("/memory/") ? '[{"role":"user"}]' : "{}"
                 end
    end
    @thread = Thread.new { @server.start }
    port = @server.listeners[0].addr[1]
    @client = Ctxmesh::Client.from_config(
      Ctxmesh::Config.new(memory_port: port, feedback_port: port, amp_port: port,
                          conversation_id: "conv-1")
    )
  end

  def teardown
    @server.shutdown
    @thread.join(2)
  end

  def test_every_route_is_actually_called
    @client.memory_get
    @client.memory_append(role: "user", content: "hi")
    @client.remember("a fact", { "topic" => "x" })
    @client.search_agent("q", top_k: 3)
    @client.knowledge_search("q", knowledge_base: "kb", top_k: 3)
    @client.skills
    @client.skill_load("s")
    @client.feedback("t1", "helpfulness", 1.0, "clear")
    @client.call_agent("other", { "q" => 1 })
    @client.call_agent_legacy("other", { "q" => 1 })
    @client.delegate("sub", { "x" => 1 })
    @client.handoff("other", include_history: true)

    %w[/memory/ /memory/agent /memory/agent/search /knowledge/search /skills /skills/load
       /feedback /amp/ /a2a/ /delegate /handoff].each do |want|
      assert @seen.any? { |s| s.split(" ", 2)[1].start_with?(want) },
             "route #{want} was never actually requested; seen=#{@seen.inspect}"
    end
  end

  def test_denied_carries_the_platforms_reason
    # A 403 is the platform refusing, not the transport failing.
    @status = 403
    err = assert_raises(Ctxmesh::DeniedError) { @client.delegate("sub", {}) }
    assert_equal 403, err.status
  end

  def test_unwired_capabilities_refuse_locally
    # The port is absent because the platform did not grant the capability. Dialling it anyway
    # turns a configuration answer into a connection refused that reads like an outage.
    bare = Ctxmesh::Client.from_config(
      Ctxmesh::Config.new(memory_port: 1, feedback_port: 1, amp_port: 1, conversation_id: "c",
                          memory_wired: false, feedback_wired: false,
                          long_term_enabled: false, knowledge_enabled: false)
    )
    assert_raises(Ctxmesh::NotWiredError) { bare.memory_get }
    assert_raises(Ctxmesh::NotWiredError) { bare.feedback("t", "d", 1.0) }
    assert_raises(Ctxmesh::NotWiredError) { bare.knowledge_search("q") }
  end

  def test_not_in_pod_is_said_plainly
    assert_raises(Ctxmesh::NotInPodError) { Ctxmesh::Config.from_env({}) }
  end

  def test_unset_port_means_not_wired_not_default
    # The distinction the whole error vocabulary rests on.
    cfg = Ctxmesh::Config.from_env({ "AGENT_NAME" => "a" })
    refute cfg.memory_wired?, "MEMORY_PORT unset must mean memory is NOT wired"
    assert_equal Ctxmesh::Config::DEFAULT_MEMORY_PORT, cfg.memory_port
  end

  def test_bad_port_is_rejected_not_defaulted
    assert_raises(Ctxmesh::NotInPodError) { Ctxmesh::Config.from_env({ "MEMORY_PORT" => "99999" }) }
  end

  def test_ambiguous_conversation_is_refused
    bare = Ctxmesh::Client.from_config(
      Ctxmesh::Config.new(memory_port: 1, feedback_port: 1, amp_port: 1, conversation_id: "")
    )
    assert_raises(Ctxmesh::Error) { bare.memory_get }
    assert_raises(Ctxmesh::Error) { @client.memory_append(role: "u", content: "x", conversation_id: "has/slash") }
  end

  def test_min_score_filters_weak_facts
    got = @client.search_agent("q", top_k: 5, min_score: 0.5)
    assert_equal 1, got.size, "minScore must drop weak hits"
    assert_equal "strong", got[0]["content"]
  end
end
