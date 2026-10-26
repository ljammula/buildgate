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
