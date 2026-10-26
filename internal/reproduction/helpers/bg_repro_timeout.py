"""Per-test timeout for a reviewer's reproduction that buildgate runs.

Loaded with `-p bg_repro_timeout` from the read-only helper directory. Each
setup, call and teardown phase of a test gets SIGALRM after
BG_REPRO_TEST_TIMEOUT seconds (60 when unset or not a positive integer);
the alarm raises BgReproTimeout, which the JUnit XML report records with
that name in the failure or error message.
"""

import os
import signal

import pytest


class BgReproTimeout(Exception):
    """The test phase ran past the factory's per-test timeout."""


def _seconds():
    try:
        value = int(os.environ.get("BG_REPRO_TEST_TIMEOUT", "60"))
    except ValueError:
        return 60
    return value if value > 0 else 60


def _on_alarm(signum, frame):
    raise BgReproTimeout("BgReproTimeout: the test ran past %d seconds" % _seconds())


def _timed():
    previous = signal.signal(signal.SIGALRM, _on_alarm)
    signal.alarm(_seconds())
    return previous


def _untimed(previous):
    signal.alarm(0)
    signal.signal(signal.SIGALRM, previous)


@pytest.hookimpl(hookwrapper=True)
def pytest_runtest_setup(item):
    previous = _timed()
    try:
        yield
    finally:
        _untimed(previous)


@pytest.hookimpl(hookwrapper=True)
def pytest_runtest_call(item):
    previous = _timed()
    try:
        yield
    finally:
        _untimed(previous)


@pytest.hookimpl(hookwrapper=True)
def pytest_runtest_teardown(item, nextitem):
    previous = _timed()
    try:
        yield
    finally:
        _untimed(previous)
