# Minitest reporter and per-test timeout for a reviewer's reproduction that
# buildgate runs. Loaded with -rbg_repro_reporter from the read-only helper
# directory; the test file is loaded by BgRepro.load_test, so a load or syntax
# error is reported as such rather than as a missing report.
#
# BG_REPRO_REPORT names the JSON report the factory reads:
#   {"tests": [{"id": "Class#test_name", "result": "pass|fail|error|timeout|skipped"}]}
# or, when the file did not load, {"load_error": "<class>: <message>"}.
# BG_REPRO_TEST_TIMEOUT is the per-test timeout in seconds (60 when unset).
require "json"
require "timeout"
require "minitest"

module BgRepro
  # Not a StandardError, so a bare rescue in a test body does not swallow it.
  class TestTimeout < Exception; end

  def self.seconds
    value = Integer(ENV.fetch("BG_REPRO_TEST_TIMEOUT", "60"), 10) rescue 60
    value > 0 ? value : 60
  end

  def self.write(doc)
    path = ENV["BG_REPRO_REPORT"]
    File.write(path, JSON.generate(doc)) if path && !path.empty?
  end

  def self.kind(result)
    failure = result.failures.first
    return "pass" if failure.nil?
    return "skipped" if failure.is_a?(Minitest::Skip)
    return "fail" unless failure.is_a?(Minitest::UnexpectedError)
    error = failure.respond_to?(:error) ? failure.error : failure.exception
    error.is_a?(TestTimeout) ? "timeout" : "error"
  end

  def self.load_test(path)
    load File.expand_path(path)
  rescue ScriptError, StandardError => e
    write("load_error" => "#{e.class}: #{e.message}"[0, 500])
    exit 2
  end

  # TimedTest runs each setup, test and teardown block under the timeout.
  module TimedTest
    def capture_exceptions
      super do
        Timeout.timeout(BgRepro.seconds, BgRepro::TestTimeout) { yield }
      end
    end
  end

  class Reporter < Minitest::AbstractReporter
    def initialize
      super()
      @tests = []
    end

    def record(result)
      klass = result.respond_to?(:klass) ? result.klass : result.class.name
      @tests << { "id" => "#{klass}##{result.name}", "result" => BgRepro.kind(result) }
    end

    def report
      BgRepro.write("tests" => @tests)
    end
  end
end

Minitest::Test.prepend(BgRepro::TimedTest)

module Minitest
  def self.plugin_bg_repro_init(_options)
    reporter << BgRepro::Reporter.new
  end
end
Minitest.extensions << "bg_repro"

require "minitest/autorun"
