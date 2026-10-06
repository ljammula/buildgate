---
name: flutter-app
description: Flutter/Dart implementation in a buildgate build round. Use when the repository has pubspec.yaml and the ticket changes Dart UI, state, services or widgets.
---

# Flutter app (buildgate)


Read `pubspec.yaml`, `analysis_options.yaml` and the repository's instructions first. Follow its localization and state-management rules; this skill does not choose them.

- Keep unit, widget, golden and integration tests distinct; add the narrowest test that covers the ticket's behaviour and run it after each change.
- Do not regenerate golden images unless the ticket asks for a visual change; you cannot inspect images here, so a regenerated golden would be unreviewed.
- Run the analyzer on what you changed. The verify command named in the build prompt's checklist is the broad check.
