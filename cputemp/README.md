# cputemp

This is a tiny tool to print the best-guess CPU temperature on Linux.

It is probably only useful to me. I simply hard-code the relevant paths/strings
for the CPUs that I actually need to use.

This tool discovers the relevant path the first time it is run and caches it for
future use so that invocations are as cheap as possible. I use cputemp for
updating the CPU temperature listing in my bar.
