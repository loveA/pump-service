
# 1. 本地构建命令
docker buildx build --platform linux/amd64,linux/arm64 -t pump-service:latest --output type=oci,dest=./pump-service-multiarch.tar  .

# 2. 导入镜像
docker load -i pump-service-multiarch.tar

# 3. 一键启动微服务
docker compose up -d

