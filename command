podman build -t stress-tester .
podman run -p8080 -p8181 stress-tester