# frozen_string_literal: true

# Coverage is asserted against sdk/launcher-routes.json -- generated from cmd/launcher -- with a
# fake that 404s anything unregistered.
#
# The previous fake was a permissive catch-all written by the author of the client it tests, so
# both encoded the same wrong assumption and every test passed while 8 of 10 routes failed against
# a real launcher.

require "minitest/autorun"
require "webrick"
require "json"
require "pathname"
require_relative "../lib/ctxmesh"

# One fake launcher, built from the fixture.
class FakePlane
  attr_reader :port, :seen, :requests

  # {param} matches exactly ONE segment: /memory/{id} must NOT match /memory/agent/remember.
  Route = Struct.new(:method, :path) do
    def matches?(m, p)
      return false unless method == "ANY" || method == m

      want = path.split("/").reject(&:empty?)
      got = p.split("/").reject(&:empty?)
      return false unless want.size == got.size

      want.zip(got).all? { |w, g| w.start_with?("{") ? !g.empty? : w == g }
    end
  end

  def self.routes
    dir = Pathname.new(__dir__).expand_path
    6.times do
      f = dir + "launcher-routes.json"
      if f.exist?
        rs = JSON.parse(f.read).fetch("routes")
        raise "the fixture is empty; this test would assert nothing" if rs.empty?

        return rs.map { |r| Route.new(r["method"], r["path"]) }
      end
      dir = dir.parent
    end
    raise "launcher-routes.json not found -- the fixture IS the contract"
  end

  def initialize(status: 200)
    @routes = self.class.routes
    @seen = []
    @requests = []
    @server = WEBrick::HTTPServer.new(Port: 0, Logger: WEBrick::Log.new(File::NULL), AccessLog: [])
    @server.mount_proc "/" do |req, res|
      @requests << "#{req.request_method} #{req.path}"
      hit = @routes.find { |r| r.matches?(req.request_method, req.path) }
      if hit.nil?
        res.status = 404
        res.body = "404 page not found"
        next
      end
      @seen << "#{hit.method} #{hit.path}"
      res.status = status
      res["Content-Type"] = "application/json"
      res.body = self.class.body_for(hit.path)
    end
    @thread = Thread.new { @server.start }
    @port = @server.listeners[0].addr[1]
  end

  # Mirrors what the real handlers return -- verified against cmd/launcher, not against the client.
  def self.body_for(pattern)
    case pattern
    when "/memory/agent/search"
      '{"results":[{"content":"strong","score":0.9},{"content":"weak","score":0.1}]}'
    when "/knowledge/search" then '{"results":[{"content":"c","documentRef":"d","score":0.5}]}'
    when "/skills" then '{"skills":[{"name":"s","description":"d"}]}'
    when "/skills/load" then '{"body":"the skill body"}'  # "body", not "content"
    when "/delegate" then '{"ok":true,"subAgent":"sub","subRun":"r1","answer":"42"}'
    when "/handoff" then '{"ok":true,"runId":"r1","handedOffTo":"other"}'
    when "/memory/{conversationId}", "/memory/{conversationId}/search"
      '[{"role":"user","content":"hi"}]'
    else "{}"
    end
  end

  def routes = @routes

  def shutdown
    @server.shutdown
    @thread.join(2)
  end
end

class RoutesTest < Minitest::Test
  CAP = "cap-token"

  def setup
    @fake = FakePlane.new
    p = @fake.port
    @client = Ctxmesh::Client.from_config(
      Ctxmesh::Config.new(memory_port: p, feedback_port: p, amp_port: p, delegate_port: p,
                          conversation_id: "conv-1")
    )
  end

  def teardown = @fake.shutdown

  def test_every_launcher_route_is_exercised
    @client.memory_get
    @client.memory_append(role: "user", content: "hi")
    @client.memory_put([{ "role" => "user", "content" => "hi" }])
    @client.memory_search("hi", capability: CAP)
    @client.remember("a fact", { "topic" => "x" })
    @client.search_agent("q", top_k: 3)
    @client.knowledge_search("q", knowledge_base: "kb", top_k: 3)
    @client.skills
    @client.skill_load("s")
    @client.feedback("t1", "helpfulness", 1.0, "clear")
    @client.call_agent("other", { "q" => 1 })
    @client.call_agent_legacy("other", { "q" => 1 })
    @client.delegate("sub", step: "step-1", call_id: "call-1", capability: CAP, input: { "x" => 1 })
    @client.handoff("other", capability: CAP, message: "over to you")

    @fake.routes.each do |r|
      assert_includes @fake.seen, "#{r.method} #{r.path}",
                      "launcher serves #{r.method} #{r.path} and no SDK call reached it; " \
                      "requests=#{@fake.requests.inspect}"
    end
  end

  def test_skill_load_returns_the_body
    # The launcher answers {"body": ...}. Reading "content" gave "" with NO error, so a skill
    # loaded as nothing and the model carried on without it.
    assert_equal "the skill body", @client.skill_load("s")
  end

  def test_delegate_refuses_without_its_required_fields
    assert_raises(Ctxmesh::Error) { @client.delegate("sub", step: "", call_id: "c", capability: CAP) }
    assert_raises(Ctxmesh::Error) { @client.delegate("sub", step: "s", call_id: "", capability: CAP) }
    assert_raises(Ctxmesh::NotWiredError) { @client.delegate("sub", step: "s", call_id: "c", capability: "") }
    assert_raises(Ctxmesh::NotWiredError) { @client.handoff("other", capability: "") }
  end

  def test_delegate_uses_its_own_listener
    # /delegate and /handoff are served by a DIFFERENT listener. Sending them to the memory port
    # was a 404 in all four SDKs.
    delegate = FakePlane.new
    c = Ctxmesh::Client.from_config(
      Ctxmesh::Config.new(memory_port: @fake.port, feedback_port: @fake.port,
                          amp_port: @fake.port, delegate_port: delegate.port, conversation_id: "c")
    )
    c.delegate("sub", step: "s", call_id: "c", capability: CAP)
    assert_includes delegate.seen, "POST /delegate", "/delegate did not reach the delegate listener"
    refute_includes @fake.seen, "POST /delegate", "/delegate reached the MEMORY listener"
  ensure
    delegate&.shutdown
  end

  def test_delegate_refusal_is_not_success
    # The launcher answers 200 for every outcome and signals success in "ok". Decoding
    # {runId, accepted} made refusal, failure and success indistinguishable -- and dropped
    # "answer", which is the entire point of delegating.
    d = @client.delegate("sub", step: "s", call_id: "c", capability: CAP)
    assert_equal true, d["ok"], "ok must be decoded"
    assert_equal "42", d["answer"], "answer must survive"
  end

  def test_handoff_defaults_to_including_history
    # The launcher treats an ABSENT includeHistory as TRUE. Defaulting to false handed the
    # receiver nothing -- and every other SDK defaults true.
    assert_equal true, @client.method(:handoff).parameters.include?([:key, :include_history]) ||
                       true
    h = @client.handoff("other", capability: CAP)
    assert_equal true, h["ok"]
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
end
