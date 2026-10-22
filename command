podman build -t stress-tester .
podman run -p8080:8080 -p8181:8181 --name ansh-stress-tester --replace stress-tester
podman exec -it running-go-app sh