import 'package:console/models.dart';
import 'package:console/request_board_filters.dart';
import 'package:flutter_test/flutter_test.dart';

RequestSummary _summary({
  required String id,
  required String state,
  String project = 'checkouts',
  String title = '',
  String workspace = '/repos/checkouts',
}) => RequestSummary(
  id: id,
  workspace: workspace,
  project: project,
  state: state,
  submittedAt: '2026-09-10T08:00:00Z',
  updatedAt: '2026-09-10T09:00:00Z',
  title: title,
);

void main() {
  group('sectionForState', () {
    test('review states are needsYou', () {
      expect(sectionForState('spec_review'), RequestBoardSection.needsYou);
      expect(sectionForState('plan_review'), RequestBoardSection.needsYou);
    });

    test('halted is needsYou, not finished (regression: the shared status '
        'vocabulary already maps halted to Status.needsHuman -- this board '
        'grouping used to disagree and put it under Finished, silently '
        'excluding a resumable request from both the section and the '
        'tab-title count)', () {
      expect(sectionForState('halted'), RequestBoardSection.needsYou);
    });

    test('drafting/building/pr_review states are working', () {
      expect(sectionForState('submitted'), RequestBoardSection.working);
      expect(sectionForState('building'), RequestBoardSection.working);
      expect(sectionForState('pr_review'), RequestBoardSection.working);
    });

    test('done, failed, and unmapped states are finished', () {
      expect(sectionForState('done'), RequestBoardSection.finished);
      expect(sectionForState('quarantined'), RequestBoardSection.finished);
      expect(sectionForState('cancelled'), RequestBoardSection.finished);
      expect(sectionForState('some_new_state'), RequestBoardSection.finished);
    });
  });

  group('sectionForRequest', () {
    RequestSummary prReview(List<String> prStates) => RequestSummary(
      id: 'r',
      workspace: '/repos/checkouts',
      project: 'checkouts',
      state: 'pr_review',
      submittedAt: '2026-09-10T08:00:00Z',
      updatedAt: '2026-09-10T09:00:00Z',
      tickets: [
        for (var i = 0; i < prStates.length; i++)
          RequestTicket(
            index: i + 1,
            prUrl: 'https://github.com/o/r/pull/${i + 1}',
            prState: prStates[i],
          ),
      ],
    );

    test(
      'a pr_review request whose PRs all wait on their reviewer needs you',
      () {
        expect(
          sectionForRequest(prReview(['ready'])),
          RequestBoardSection.needsYou,
        );
        expect(
          sectionForRequest(prReview(['merged', 'ready'])),
          RequestBoardSection.needsYou,
        );
      },
    );

    test('a stacked PR (waiting on its base PR merging) also needs you', () {
      expect(
        sectionForRequest(prReview(['ready', 'stacked'])),
        RequestBoardSection.needsYou,
      );
    });

    test(
      'a pr_review request with a draft or approved PR is still working',
      () {
        expect(
          sectionForRequest(prReview(['draft'])),
          RequestBoardSection.working,
        );
        expect(
          sectionForRequest(prReview(['ready', 'approved'])),
          RequestBoardSection.working,
        );
        expect(sectionForRequest(prReview([])), RequestBoardSection.working);
      },
    );

    test('other states keep their state-based section', () {
      expect(
        sectionForRequest(_summary(id: 'a', state: 'building')),
        RequestBoardSection.working,
      );
      expect(
        sectionForRequest(_summary(id: 'b', state: 'done')),
        RequestBoardSection.finished,
      );
    });
  });

  group('section filter', () {
    final requests = [
      _summary(id: 'waiting', state: 'plan_review'),
      _summary(id: 'busy', state: 'building'),
      _summary(id: 'shipped', state: 'done'),
      _summary(id: 'stopped', state: 'quarantined'),
    ];
    List<String> matching(RequestBoardSection section) => [
      for (final r in requests)
        if (matchesRequestBoardFilters(
          r,
          RequestBoardFilters(section: section),
        ))
          r.id,
    ];

    test('each section shows only its own requests', () {
      expect(matching(RequestBoardSection.needsYou), ['waiting']);
      expect(matching(RequestBoardSection.working), ['busy']);
      expect(matching(RequestBoardSection.finished), ['shipped', 'stopped']);
    });
  });

  group('RequestBoardFilters URL round-trip', () {
    test('an empty filter set produces no query parameters', () {
      const filters = RequestBoardFilters();
      expect(filters.toQueryParameters(), isEmpty);
      expect(RequestBoardFilters.fromUri(Uri(path: '/')), filters);
    });

    test('every filter field round-trips through a URI', () {
      const filters = RequestBoardFilters(
        projects: {'checkouts', 'billing'},
        section: RequestBoardSection.needsYou,
        search: 'idempotency',
      );

      final uri = Uri(path: '/', queryParameters: filters.toQueryParameters());
      final restored = RequestBoardFilters.fromUri(uri);

      expect(restored, filters);
    });

    test('a project name containing a comma round-trips as one project, not '
        'two (regression: a single comma-joined "project" value can\'t '
        'distinguish a literal comma in a name from the delimiter)', () {
      const filters = RequestBoardFilters(
        projects: {'billing,legacy', 'checkouts'},
      );

      final uri = Uri(path: '/', queryParameters: filters.toQueryParameters());
      final restored = RequestBoardFilters.fromUri(uri);

      expect(restored.projects, {'billing,legacy', 'checkouts'});
    });

    test('an unrecognized group query value is ignored, not crashed on', () {
      final uri = Uri(path: '/', queryParameters: {'group': 'bogus'});
      expect(RequestBoardFilters.fromUri(uri).section, isNull);
    });
  });

  group('matchesRequestBoardFilters', () {
    final review = _summary(
      id: 'r1',
      state: 'spec_review',
      project: 'checkouts',
    );
    final working = _summary(id: 'r2', state: 'building', project: 'billing');

    test('no filters matches everything', () {
      const filters = RequestBoardFilters();
      expect(matchesRequestBoardFilters(review, filters), isTrue);
      expect(matchesRequestBoardFilters(working, filters), isTrue);
    });

    test('a project filter excludes other projects', () {
      const filters = RequestBoardFilters(projects: {'checkouts'});
      expect(matchesRequestBoardFilters(review, filters), isTrue);
      expect(matchesRequestBoardFilters(working, filters), isFalse);
    });

    test('a section filter excludes other sections', () {
      const filters = RequestBoardFilters(section: RequestBoardSection.working);
      expect(matchesRequestBoardFilters(review, filters), isFalse);
      expect(matchesRequestBoardFilters(working, filters), isTrue);
    });

    test(
      'free-text search matches id, title, or workspace case-insensitively',
      () {
        final titled = _summary(
          id: 'r3',
          state: 'done',
          title: 'Add idempotency keys',
        );
        expect(
          matchesRequestBoardFilters(
            titled,
            const RequestBoardFilters(search: 'IDEMPOTENCY'),
          ),
          isTrue,
        );
        expect(
          matchesRequestBoardFilters(
            titled,
            const RequestBoardFilters(search: 'nonexistent'),
          ),
          isFalse,
        );
      },
    );
  });

  test('distinctProjects sorts and de-duplicates', () {
    final requests = [
      _summary(id: 'a', state: 'done', project: 'billing'),
      _summary(id: 'b', state: 'done', project: 'checkouts'),
      _summary(id: 'c', state: 'done', project: 'billing'),
    ];
    expect(distinctProjects(requests), ['billing', 'checkouts']);
  });
}
