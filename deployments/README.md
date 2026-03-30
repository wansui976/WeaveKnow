# Docker 部署说明

本目录已包含完整 Docker 部署文件：

- `Dockerfile`：构建后端镜像
- `docker-compose.yaml`：一键启动基础设施 + 后端服务
- `config.docker.yaml`：容器内网络环境使用的后端配置

## 1. 准备配置

编辑 `deployments/config.docker.yaml`，至少补齐：

- `embedding.api_key`
- `llm.api_key`

> 如果你使用本地模型或代理，请对应修改 `llm.base_url` / `embedding.base_url`。

## 2. 启动

在项目根目录执行：

```bash
docker compose -f deployments/docker-compose.yaml up -d --build
```

## 3. 查看状态

```bash
docker compose -f deployments/docker-compose.yaml ps
docker compose -f deployments/docker-compose.yaml logs -f backend
```

后端默认暴露端口：`8081`

## 4. 停止

```bash
docker compose -f deployments/docker-compose.yaml down
```

若需清空数据卷：

```bash
docker compose -f deployments/docker-compose.yaml down -v
```
