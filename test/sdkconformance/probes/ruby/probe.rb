# frozen_string_literal: true

# Drives the Ruby SDK's public API against a real launcher, one call per contract route, and prints
# "PASS|FAIL|MISSING <route>[: reason]" for each. hack/sdk-conformance runs it and checks every PASS
# against what the launcher actually received.

require "json"
require "ctxmesh"

contract = JSON.parse(File.read(ENV.fetch("CTXMESH_PROBE_CONTRACT")))
capability = ENV.fetch("CTXMESH_PROBE_CAPABILITY", "")

def segment(golden, index) = golden["request"]["path"].delete_prefix("/").split("/")[index].to_s

def body(golden) = golden["request"]["body"]

def zero?(v) = v.nil? || v == "" || v == false || v == 0 || v == {}

# Every value the SDK decoded must appear, equal, at the same place in the launcher's golden
# answer; arrays must have the same length. Returns where they first differ, or nil.
def differs_at(got, want, at = "$")
  case got
  when Hash
    return at unless want.is_a?(Hash)

    got.each do |k, v|
      next if zero?(v) && !want.key?(k)

      where = differs_at(v, want[k], "#{at}.#{k}")
      return where if where
    end
    nil
  when Array
    return at unless want.is_a?(Array) && want.size == got.size

    got.each_with_index do |v, i|
      where = differs_at(v, want[i], "#{at}[#{i}]")
      return where if where
    end
    nil
  else
    got == want ? nil : at
  end
end

# The current route's sdkResult: the response fields every SDK's result must carry.
$required = []

def same?(got, want) = differs_at(got, want).nil? && differs_at(want, got).nil?

# Whether got holds want's value at a sdkResult path ("[]" descends into each element). Returns
# where it does not, or nil.
def missing_at(got, want, segs, at = "$")
  return(same?(got, want) ? nil : at) if segs.empty?

  each = segs.first.end_with?("[]")
  key = each ? segs.first.delete_suffix("[]") : segs.first
  unless key.empty?
    return "#{at}.#{key}" unless got.is_a?(Hash) && got.key?(key)

    got, want, at = got[key], (want || {})[key], "#{at}.#{key}"
  end
  return missing_at(got, want, segs.drop(1), at) unless each
  return at unless got.is_a?(Array) && want.is_a?(Array) && got.size == want.size

  got.each_with_index do |g, i|
    where = missing_at(g, want[i], segs.drop(1), "#{at}[#{i}]")
    return where if where
  end
  nil
end

def expect(what, got, golden)
  want = golden["response"]["body"]
  where = differs_at(got, want)
  unless where.nil?
    raise "#{what} decoded as #{JSON.generate(got)}, but the launcher sent #{JSON.generate(want)} (differs at #{where})"
  end

  # differs_at compares only what the SDK kept, so it passes an SDK that dropped a field.
  $required.each do |path|
    missed = path == "$" ? (same?(got, want) ? nil : "$") : missing_at(got, want, path.split("."))
    next if missed.nil?

    raise "#{what} decoded as #{JSON.generate(got)}, which does not carry #{path} with the launcher's value " \
          "(at #{missed}); the launcher sent #{JSON.generate(want)}"
  end
end

calls = {
  "POST /a2a/{targetAgent}" => lambda { |c, g|
    expect("reply", c.call_agent_legacy(segment(g, 1), body(g)), g)
  },
  "POST /amp/{targetAgent}" => lambda { |c, g|
    expect("reply", c.call_agent(segment(g, 1), body(g)), g)
  },
  "POST /delegate" => lambda { |c, g|
    b = body(g)
    d = c.delegate(b["subAgent"], step: b["step"], call_id: b["callId"], capability: capability, input: b["input"])
    expect("delegation", d, g)
  },
  "ANY /feedback" => lambda { |c, g|
    b = body(g)
    c.feedback(b["traceId"], b["name"], b["value"], b["comment"])
  },
  "POST /handoff" => lambda { |c, g|
    b = body(g)
    h = c.handoff(b["targetAgent"], capability: capability, message: b["message"],
                                    include_history: b.fetch("includeHistory", true))
    expect("handoff", h, g)
  },
  "POST /knowledge/search" => lambda { |c, g|
    b = body(g)
    chunks = c.knowledge_search(b["query"], knowledge_base: b["knowledgeBase"], top_k: b.fetch("topK", 5))
    expect("chunks", { "results" => chunks }, g)
  },
  "POST /memory/agent/remember" => lambda { |c, g|
    b = body(g)
    c.remember(b["content"], b["tags"] || {})
  },
  "POST /memory/agent/search" => lambda { |c, g|
    b = body(g)
    expect("facts", { "results" => c.search_agent(b["query"], top_k: b.fetch("topK", 5)) }, g)
  },
  "GET /memory/{conversationId}" => lambda { |c, g|
    expect("entries", c.memory_get(segment(g, 1)), g)
  },
  "PUT /memory/{conversationId}" => lambda { |c, g|
    c.memory_put(body(g), conversation_id: segment(g, 1))
  },
  "POST /memory/{conversationId}/append" => lambda { |c, g|
    b = body(g)
    c.memory_append(role: b["role"], content: b["content"], conversation_id: segment(g, 1))
  },
  "GET /memory/{conversationId}/search" => lambda { |c, g|
    q = (g["request"]["query"] || {})["q"].to_s
    expect("entries", c.memory_search(q, conversation_id: segment(g, 1)), g)
  },
  "GET /skills" => lambda { |c, g|
    expect("skills", { "skills" => c.skills }, g)
  },
  "POST /skills/load" => lambda { |c, g|
    expect("skill", { "body" => c.skill_load(body(g)["name"]) }, g)
  }
}

# from_env, not an explicit Config: the env the launcher publishes is part of what is checked.
begin
  client = Ctxmesh::Client.from_env(ENV)
  config_error = nil
rescue StandardError => e
  config_error = e
end

contract.fetch("routes").each do |route|
  key = "#{route['method']} #{route['path']}"
  call = calls[key]
  if config_error
    puts "FAIL #{key}: the SDK does not configure from the launcher's env: #{config_error.message}"
  elsif call.nil?
    puts "MISSING #{key}: the Ruby SDK has no call for it"
  else
    begin
      $required = route.fetch("sdkResult", [])
      call.call(client, route["goldens"].first)
      puts "PASS #{key}"
    rescue StandardError => e
      puts "FAIL #{key}: #{e.class}: #{e.message.gsub(/\s+/, ' ')}"
    end
  end
end
