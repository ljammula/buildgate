Hand-written, not recorded: Maven and a JDK are not installed on the machine
that recorded the other runners' fixtures. Each file follows Surefire 3's
TEST-<class>.xml format (surefire-test-report.xsd) as the JUnit Platform
provider writes it: a failed assertion is a <failure> of type
org.opentest4j.AssertionFailedError, an exception an <error>, JUnit Jupiter's
default timeout an <error> of type java.util.concurrent.TimeoutException,
@Disabled a <skipped>, and a parameterized invocation a case named
"method(args)[n]". Surefire writes no report for a module that does not
compile, which the runner reads as missing. Replace these with recorded runs
when a JDK is available.

Recorded: timeout-separate-thread.xml is a real Surefire 3.2.5 report (JUnit
Jupiter 5.10.2, Temurin 21, in the maven:3.9-eclipse-temurin-21 image, its
<properties> block dropped) of a test whose busy loop never checks for an
interrupt, run with the runner's command and a 5 s default timeout in
SEPARATE_THREAD mode: the test reports a TimeoutException and the JVM exits.
The same command over a test class that calls a method the main sources do
not have exits 86 from its `mvn test-compile` step and writes no report,
which the runner reads as a compile error.
