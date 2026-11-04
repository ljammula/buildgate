#!/usr/bin/perl
# A plain Perl test: no test framework, a non-zero exit is a failure.
use strict;
use warnings;
use Calc;

my $got = Calc::add(2, 3);
die "add(2, 3) = $got, want 5\n" unless $got == 5;
print "ok add\n";
