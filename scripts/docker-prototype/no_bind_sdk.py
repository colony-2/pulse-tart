"""SDK qualification helpers for the disposable macOS guest."""
import shlex


def qualify(h, mac):
    worker = 'cd /var/tmp && sudo -u pulseworker env HOME=/Users/pulseworker PATH=/opt/pulse-docker:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin DOCKER_HOST=unix:///var/run/docker.sock TESTCONTAINERS_DOCKER_SOCKET_OVERRIDE=/var/run/docker.sock '
    h.guest(mac, worker+'/usr/bin/python3 -m venv /Users/pulseworker/sdk')
    h.guest(mac, worker+'/Users/pulseworker/sdk/bin/pip install docker==7.1.0', timeout=180)
    (h.OUT/'macos-sdk.json').write_bytes(h.guest(mac, worker+'/Users/pulseworker/sdk/bin/python /opt/pulse-docker/sdk.py'))
    h.result('no_bind_macos_sdk','passed','Official Docker Python SDK 7.1.0, concurrent requests, run API; Go Testcontainers with Ryuk passed in relay tests.')
    # Tools are installed only inside the disposable primary, never on the host.
    print('Preparing Java Testcontainers tools inside disposable macOS guest', flush=True)
    h.guest(mac, 'HOMEBREW_NO_AUTO_UPDATE=1 /opt/homebrew/bin/brew install openjdk@21 maven', timeout=900)
    h.guest(mac, worker+'mkdir -p /Users/pulseworker/java/src/main/java')
    for name in ['pom.xml','src/main/java/Probe.java']:
        h.upload(mac, '/Users/pulseworker/java/'+name, (h.ROOT/'experimental/docker/testcontainers-java'/name).read_bytes())
    h.guest(mac, 'sudo chown -R pulseworker:pulseworker /Users/pulseworker/java')
    command='cd /Users/pulseworker/java && export JAVA_HOME=/opt/homebrew/opt/openjdk@21/libexec/openjdk.jdk/Contents/Home && export PATH="$JAVA_HOME/bin:/opt/homebrew/bin:$PATH" && mvn -q compile dependency:build-classpath -Dmdep.outputFile=classpath && java -cp "target/classes:$(cat classpath)" Probe'
    (h.OUT/'macos-testcontainers-java.log').write_bytes(h.guest(mac,worker+'bash -c '+shlex.quote(command),timeout=900))
    h.result('no_bind_macos_java','passed','Java Testcontainers 2.0.5, copied fixture and TCP service, normal Ryuk enabled.')
