build: clean
	GOOS=linux go build -o main .
	docker build -t eggsbenjamin/zeebe_nomad_long_running_process .
	docker push eggsbenjamin/zeebe_nomad_long_running_process

clean:
	rm ./main
