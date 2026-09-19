## Run
```shell
dapr run --app-id supertrade-catalog --app-port 8101 --dapr-http-port 3101 --dapr-grpc-port 6101 --placement-host-address 172.12.1.1:50005 --scheduler-host-address 172.12.1.1:50006 --config F:\go\src\github.com\YunBright\deployer\dapr\config.yaml -- go run .\cmd\catalog
```