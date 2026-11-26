#!/usr/bin/env bash
# Spike 07 watcher, run as root during a scenario. About every 200 ms it reads
# every /proc/<pid>/cmdline and /proc/<pid>/environ and looks for the values
# in the pattern files of a directory; each file is one class (its name). The
# values stay in the files and in perl's memory, so no argv ever holds one. A
# hit is recorded as class, kind, pid, host uid and comm only.
#
# The owner comes from fstat on the same open file that is read, so a process
# that exits right after the read is still attributed (a grep-then-stat
# watcher lost two short-lived hits that way on 26 November). A process that
# has exited reads as empty and cannot match.
# Usage: watch.sh <pattern-dir> <out-file> <stop-file>
set -u
exec perl -e '
use strict;
use warnings;
my ($dir, $out, $stop) = @ARGV;
my %pat;
opendir(my $dh, $dir) or die "watch: $dir: $!\n";
for my $f (sort grep { !/^\./ && -f "$dir/$_" } readdir $dh) {
	open(my $fh, "<", "$dir/$f") or next;
	my @v = grep { length } map { s/\r?\n\z//r } <$fh>;
	$pat{$f} = \@v if @v;
}
closedir $dh;
my (%seen, $scans);
$scans = 0;
until (-e $stop) {
	$scans++;
	opendir(my $p, "/proc") or die "watch: /proc: $!\n";
	for my $pid (grep { /^\d+\z/ } readdir $p) {
		for my $kind ("cmdline", "environ") {
			open(my $fh, "<", "/proc/$pid/$kind") or next;
			my $uid = (stat $fh)[4];
			my $data = do { local $/; <$fh> };
			close $fh;
			next unless defined $data && length $data;
			CLASS: for my $class (sort keys %pat) {
				for my $v (@{ $pat{$class} }) {
					next if index($data, $v) < 0;
					my $key = "$class $kind $pid";
					if (!exists $seen{$key}) {
						my $comm = "";
						if (open(my $c, "<", "/proc/$pid/comm")) {
							$comm = <$c> // "";
							close $c;
						}
						$comm =~ s/\n\z//;
						$comm =~ tr{A-Za-z0-9._:/-}{_}c;
						$comm = substr($comm, 0, 32);
						$seen{$key} = "hit class=$class kind=$kind pid=$pid uid=" . ($uid // "?") . " comm=" . (length $comm ? $comm : "?");
					}
					next CLASS;
				}
			}
		}
	}
	closedir $p;
	select(undef, undef, undef, 0.2);
}
open(my $o, ">", $out) or die "watch: $out: $!\n";
print $o "scans=$scans\n";
print $o "$_\n" for sort values %seen;
close $o;
' "$@"
