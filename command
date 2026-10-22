podman build -t stress-tester .
podman run --platform linux/amd64 -p8080:8080 -p8181:8181 -name ansh-stress-tester --replace stress-tester