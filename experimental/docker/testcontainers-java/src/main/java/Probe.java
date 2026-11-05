import org.testcontainers.containers.GenericContainer;
import org.testcontainers.containers.wait.strategy.Wait;
import java.net.URI;
import java.net.http.*;
public class Probe {
 public static void main(String[] args) throws Exception {
  try (GenericContainer<?> c = new GenericContainer<>("python:3.13-alpine").withExposedPorts(8080).withCommand("python", "-m", "http.server", "8080").waitingFor(Wait.forHttp("/"))) {
   c.start();
   var response=HttpClient.newHttpClient().send(HttpRequest.newBuilder(URI.create("http://"+c.getHost()+":"+c.getMappedPort(8080))).build(),HttpResponse.BodyHandlers.ofString());
   if(response.statusCode()!=200) throw new AssertionError("HTTP "+response.statusCode());
   System.out.println("PULSE_JAVA_TESTCONTAINERS_OK");
  }
 }
}
