import org.testcontainers.containers.GenericContainer;
import org.testcontainers.containers.wait.strategy.Wait;
import java.net.URI;
import org.testcontainers.images.builder.Transferable;
import java.net.http.*;
public class Probe {
 public static void main(String[] args) throws Exception {
  try (GenericContainer<?> c = new GenericContainer<>("python:3.13-alpine").withExposedPorts(8080).withCopyToContainer(Transferable.of("copied-fixture"), "/www/index.html").withCommand("python", "-m", "http.server", "8080", "--directory", "/www").waitingFor(Wait.forHttp("/"))) {
   c.start();
   var response=HttpClient.newHttpClient().send(HttpRequest.newBuilder(URI.create("http://"+c.getHost()+":"+c.getMappedPort(8080))).build(),HttpResponse.BodyHandlers.ofString());
   if(response.statusCode()!=200 || !response.body().equals("copied-fixture")) throw new AssertionError("HTTP "+response.statusCode());
   System.out.println("PULSE_JAVA_TESTCONTAINERS_OK");
  }
 }
}
