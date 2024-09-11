all:
	CGO_ENABLED=0 go build -v -o hcloud-ip && strip hcloud-ip
