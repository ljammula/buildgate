import 'package:console/approve_reject.dart';
import 'package:console/operator_identity.dart';
import 'package:console/operator_identity_stub.dart';
import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';

void main() {
  setUp(clearOperatorNameForTest);

  // C2 (operator demo, 2026-09-26): the operator-name dialog's helperText
  // was ellipsized to one line even though its copy runs longer than that
  // -- helperMaxLines: 3 lets it actually be read.
  testWidgets('the operator-name dialog helper text allows multiple lines', (
    tester,
  ) async {
    await tester.pumpWidget(
      MaterialApp(
        home: Builder(
          builder: (context) => ElevatedButton(
            onPressed: () => ensureOperatorName(context),
            child: const Text('prompt'),
          ),
        ),
      ),
    );

    await tester.tap(find.text('prompt'));
    await tester.pumpAndSettle();

    final field = tester.widget<TextField>(
      find.byKey(const ValueKey('operator-name-field')),
    );
    expect(field.decoration?.helperMaxLines, 3);
  });
}
