# 沙箱运行时镜像：Python 3.13 + Node.js 22 + ffmpeg
FROM node:22-bookworm-slim AS node

FROM python:3.13-slim-bookworm

ENV DEBIAN_FRONTEND=noninteractive \
    PYTHONDONTWRITEBYTECODE=1 \
    PYTHONUNBUFFERED=1 \
    PIP_NO_CACHE_DIR=1 \
    PIP_DISABLE_PIP_VERSION_CHECK=1

# 构建时可指定镜像源加速，例如：
#   --build-arg APT_MIRROR=https://mirrors.aliyun.com --build-arg PIP_INDEX_URL=https://mirrors.aliyun.com/pypi/simple/
ARG APT_MIRROR=
ARG PIP_INDEX_URL=

RUN if [ -n "$APT_MIRROR" ]; then \
        sed -i "s|http://deb.debian.org|${APT_MIRROR}|g" /etc/apt/sources.list.d/debian.sources; \
    fi \
    && apt-get update \
    && apt-get install -y --no-install-recommends \
        ffmpeg \
        ca-certificates \
        fonts-wqy-microhei \
        procps \
    && rm -rf /var/lib/apt/lists/*

# 办公文档相关的系统工具：
#   libreoffice  docx / xlsx / pptx 与 pdf 等格式互转（soffice --headless --convert-to）
#   poppler      pdf2image、pdftotext 依赖
#   pandoc       markdown / html / docx 互转（pypandoc 依赖）
#   tesseract    OCR，含简体中文
#   pango        weasyprint（HTML 转 PDF）依赖
RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        libreoffice-writer \
        libreoffice-calc \
        libreoffice-impress \
        poppler-utils \
        pandoc \
        tesseract-ocr \
        tesseract-ocr-chi-sim \
        libpango-1.0-0 \
        libpangoft2-1.0-0 \
        fonts-wqy-zenhei \
    && rm -rf /var/lib/apt/lists/*

# 常用命令行工具与数据库客户端（git 走 HTTPS_PROXY，只支持 https 克隆，ssh 方式不可用）
RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        git \
        curl \
        wget \
        zip \
        unzip \
        jq \
        sqlite3 \
        postgresql-client \
        default-mysql-client \
        redis-tools \
    && rm -rf /var/lib/apt/lists/*

COPY --from=node /usr/local/bin/node /usr/local/bin/node
COPY --from=node /usr/local/lib/node_modules /usr/local/lib/node_modules
RUN ln -s ../lib/node_modules/npm/bin/npm-cli.js /usr/local/bin/npm \
    && ln -s ../lib/node_modules/npm/bin/npx-cli.js /usr/local/bin/npx

COPY requirements.txt /tmp/requirements.txt
RUN pip install ${PIP_INDEX_URL:+--index-url $PIP_INDEX_URL} -r /tmp/requirements.txt && rm /tmp/requirements.txt

# matplotlib 默认字体不支持中文，图表里的中文会显示成方块
RUN printf 'font.sans-serif: WenQuanYi Micro Hei, DejaVu Sans\naxes.unicode_minus: False\n' > /etc/matplotlibrc
ENV MATPLOTLIBRC=/etc/matplotlibrc

# 工作目录是宿主机挂载的，属主可能和沙箱用户对不上，git 会报 dubious ownership；
# 沙箱里只有一个用户，信任所有目录。默认提交身份避免 git commit 因缺少 user.name 失败
RUN git config --system --add safe.directory '*' \
    && git config --system user.name sandbox \
    && git config --system user.email sandbox@localhost \
    && git config --system init.defaultBranch main

RUN groupadd -g 1000 sandbox \
    && useradd -u 1000 -g 1000 -M -d /tmp -s /bin/bash sandbox \
    && mkdir -p /opt/workspace /opt/user \
    && chown 1000:1000 /opt/workspace /opt/user

# 根文件系统是只读的：HOME 和各种缓存放 /tmp(tmpfs)。
# /opt/workspace 是当前对话的目录，/opt/user 是用户长期空间：
# pip install --user 装到用户空间，所有对话共用；npm install 装到当前对话目录
ENV HOME=/tmp \
    MPLCONFIGDIR=/tmp/.matplotlib \
    NPM_CONFIG_CACHE=/tmp/.npm \
    PYTHONUSERBASE=/opt/user/.local \
    PATH=/opt/user/.local/bin:/opt/workspace/node_modules/.bin:$PATH

WORKDIR /opt/workspace
USER 1000:1000
CMD ["sleep", "infinity"]
