# frozen_string_literal: true

require "json"
require "net/http"
require "uri"

require_relative "ctxmesh/version"
require_relative "ctxmesh/errors"
require_relative "ctxmesh/config"

# Ruby SDK for agents running on ctxmesh, the Kubernetes-native control plane for AI agents.
#
# An agent runs in a pod beside the platform's sidecars, and this gem is the typed way to reach
# them over localhost: conversation memory, long-term memory, knowledge bases, skills, feedback,
# agent-to-agent calls, delegation and handoff.
#
# It holds no credentials. Endpoints and identity arrive in the environment the platform
# injects, so there is no API key to manage and no base URL to configure.
#
# Conformance tier: plane-client (ADR 0139). The managed agent loop and model client are
# authoring-tier and live in the Python and TypeScript SDKs.
module Ctxmesh
  # The entry point.
  class Client
    DEFAULT_TIMEOUT = 15
    # Search may wait on an embedding call through the token-service.
    SEARCH_TIMEOUT = 60

    attr_reader :config

    def initialize(config)
      @config = config
    end

    # Reads the launcher environment. Raises NotInPodError outside a ctxmesh pod.
    def self.from_env(env = ENV) = new(Config.from_env(env))

    # Builds a client from an explicit config — for tests and offline work.
    def self.from_config(config) = new(config)

    # ── memory: /memory and /memory/agent ───────────────────────────────────

    # Returns the conversation so far.
    def memory_get(conversation_id = nil)
      require_memory!
      body = request("GET", "#{@config.memory_base}/memory/#{escape(conv(conversation_id))}")
      body.is_a?(Array) ? body : []
    end

    # Adds one entry to the conversation.
    def memory_append(role:, content:, conversation_id: nil)
      require_memory!
      request("POST", "#{@config.memory_base}/memory/#{escape(conv(conversation_id))}",
              { role: role, content: content })
      nil
    end

    # Replaces the conversation wholesale.
    def memory_put(entries, conversation_id: nil)
      require_memory!
      request("PUT", "#{@config.memory_base}/memory/#{escape(conv(conversation_id))}", entries)
      nil
    end

    # Writes a fact to this agent's long-term memory.
    def remember(content, tags = {})
      require_long_term!
      payload = { content: content }
      payload[:tags] = tags unless tags.nil? || tags.empty?
      request("POST", "#{@config.memory_base}/memory/agent", payload)
      nil
    end

    # Retrieves facts from long-term memory; min_score drops weak matches.
    def search_agent(query, top_k: 5, min_score: 0.0)
      require_long_term!
      body = request("POST", "#{@config.memory_base}/memory/agent/search",
                     { query: query, topK: top_k.positive? ? top_k : 5 })
      results(body).select { |f| f.fetch("score", 0.0) >= min_score }
    end

    # ── knowledge: /knowledge/search ────────────────────────────────────────

    # Retrieval over the granted knowledge bases; knowledge_base may be nil for all of them.
    def knowledge_search(query, knowledge_base: nil, top_k: 5)
      unless @config.knowledge_enabled?
        raise NotWiredError, "ctxmesh: knowledge is not enabled (KNOWLEDGE_BASE_ENABLED)"
      end

      payload = { query: query, topK: top_k.positive? ? top_k : 5 }
      payload[:knowledgeBase] = knowledge_base if knowledge_base && !knowledge_base.empty?
      results(request("POST", "#{@config.memory_base}/knowledge/search", payload, SEARCH_TIMEOUT))
    end

    # ── skills: /skills and /skills/load ────────────────────────────────────

    # The skills the platform attached to this agent.
    def skills
      body = request("GET", "#{@config.memory_base}/skills")
      body.is_a?(Hash) ? body.fetch("skills", []) : []
    end

    # Fetches a skill's body by name.
    def skill_load(name)
      body = request("POST", "#{@config.memory_base}/skills/load", { name: name })
      body.is_a?(Hash) ? body.fetch("content", "") : ""
    end

    # ── feedback: /feedback ─────────────────────────────────────────────────

    # Records a score against a trace — the signal that drives evals and canary promotion.
    def feedback(trace_id, dimension, score, comment = nil)
      unless @config.feedback_wired?
        raise NotWiredError, "ctxmesh: feedback is not wired (FEEDBACK_PORT unset)"
      end

      payload = { traceId: trace_id, dimension: dimension, score: score }
      payload[:comment] = comment if comment && !comment.empty?
      request("POST", "#{@config.feedback_base}/feedback", payload)
      nil
    end

    # ── mesh: /amp and /a2a ─────────────────────────────────────────────────

    # Invokes another agent through AMP.
    def call_agent(target_agent, payload)
      require_target!(target_agent)
      request("POST", "#{@config.amp_base}/amp/#{escape(target_agent)}", payload)
    end

    # Invokes another agent through the retired /a2a path. Prefer #call_agent.
    #
    # Kept because the launcher still serves it (ADR 0138); an SDK that pretends a served route
    # does not exist is the drift the contract gate exists to prevent.
    def call_agent_legacy(target_agent, payload)
      require_target!(target_agent)
      request("POST", "#{@config.amp_base}/a2a/#{escape(target_agent)}", payload)
    end

    # ── runs: /delegate and /handoff ────────────────────────────────────────

    # Spawns a sub-run on another agent.
    #
    # The platform fences this — spawn depth, total spawns and budget are enforced on its side,
    # so a refusal arrives as DeniedError rather than a silently dropped call.
    def delegate(sub_agent, input)
      raise Error, "ctxmesh: sub-agent is required" if sub_agent.nil? || sub_agent.strip.empty?

      request("POST", "#{@config.memory_base}/delegate", { subAgent: sub_agent, input: input })
    end

    # Transfers the conversation to another agent.
    def handoff(target_agent, include_history: false)
      require_target!(target_agent)
      request("POST", "#{@config.memory_base}/handoff",
              { targetAgent: target_agent, includeHistory: include_history })
      nil
    end

    private

    def require_memory!
      return if @config.memory_wired?

      raise NotWiredError, "ctxmesh: memory is not wired (MEMORY_PORT unset)"
    end

    def require_long_term!
      require_memory!
      return if @config.long_term_enabled?

      raise NotWiredError, "ctxmesh: long-term memory is not enabled (MEMORY_LONGTERM_ENABLED)"
    end

    def require_target!(t)
      raise Error, "ctxmesh: target agent is required" if t.nil? || t.strip.empty?
    end

    # Explicit id first, else the injected CONVERSATION_ID. Empty is an error rather than a
    # silent write to a shared bucket.
    def conv(id)
      v = id.nil? || id.to_s.strip.empty? ? @config.conversation_id : id.to_s
      raise Error, "ctxmesh: no conversation id (pass one, or set CONVERSATION_ID)" if v.strip.empty?
      if v.include?("/") || v.match?(/\s/)
        raise Error, "ctxmesh: conversation id #{v.inspect} contains a separator or whitespace"
      end

      v
    end

    def results(body) = body.is_a?(Hash) ? body.fetch("results", []) : []

    def escape(s) = URI.encode_www_form_component(s)

    def request(method, url, payload = nil, timeout = DEFAULT_TIMEOUT)
      uri = URI.parse(url)
      req = Net::HTTP.const_get(method.capitalize).new(uri)
      req["Accept"] = "application/json"
      if payload
        req["Content-Type"] = "application/json"
        req.body = JSON.generate(payload)
      end

      resp = Net::HTTP.start(uri.hostname, uri.port, open_timeout: 5, read_timeout: timeout) do |http|
        http.request(req)
      end

      code = resp.code.to_i
      text = (resp.body || "").strip
      raise DeniedError.new(uri.path, text) if code == 403
      raise ApiError.new(code, uri.path, text) unless (200..299).cover?(code)
      return nil if text.empty?

      JSON.parse(text)
    rescue JSON::ParserError => e
      raise Error, "ctxmesh: decode response from #{uri.path}: #{e.message}"
    rescue SystemCallError, Timeout::Error, IOError => e
      raise Error, "ctxmesh: #{uri.path}: #{e.message}"
    end
  end
end
