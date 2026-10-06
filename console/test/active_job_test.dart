import 'package:console/models.dart';
import 'package:flutter_test/flutter_test.dart';

RequestSummary _request(String state, Map<String, dynamic>? activeJob) =>
    RequestSummary.fromJson({
      'id': 'req-1',
      'workspace': '/w',
      'project': 'p',
      'state': state,
      'submitted_at': '2026-09-28T00:00:00Z',
      'updated_at': '2026-09-28T00:00:00Z',
      if (activeJob != null) 'active_job': activeJob,
    });

const _planningJob = {
  'stage': 'planning',
  'role': 'planning',
  'model': 'luna',
  'model_id': 'gpt-5.6-luna',
  'harness': 'pi',
  'thinking': 'max',
  'route': 'codex',
  'started_at': '2026-09-28T02:32:00Z',
};

void main() {
  test('a running job parses and labels role, model, harness, effort and route', () {
    final job = _request('planning', _planningJob).runningJob;
    expect(job, isNotNull);
    expect(
      job!.label,
      'planning role · luna (gpt-5.6-luna) · harness pi · thinking max · route codex',
    );
  });

  test('a job left over from another stage is never shown as running', () {
    expect(_request('plan_review', _planningJob).runningJob, isNull);
  });

  test('no active_job means nothing running', () {
    expect(_request('planning', null).runningJob, isNull);
  });

  test('the label drops empty parts and a model id equal to its name', () {
    const job = ActiveJob(
      stage: 'spec_drafting',
      model: 'qwen',
      modelId: 'qwen',
    );
    expect(job.label, 'qwen');
  });
}
