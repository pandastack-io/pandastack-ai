# PandaStack template · code-interpreter
#
# Enterprise-grade sandbox: Python 3.11 + Node.js 22 LTS + full data-science /
# AI / Playwright stack. Used for both interactive code execution and Functions.
#
# Bake:
#   pandastack template build -f Dockerfile -n code-interpreter --size-mb 12288

FROM python:3.11

ENV DEBIAN_FRONTEND=noninteractive \
    PLAYWRIGHT_BROWSERS_PATH=/opt/playwright

# ── System packages ───────────────────────────────────────────────────────────
RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates curl wget git \
    procps net-tools iproute2 build-essential \
    libpq-dev gcc xz-utils \
    # Native deps for opencv / librosa / scipy
    libgl1 libglib2.0-0 libsndfile1 libsm6 libxext6 libxrender-dev ffmpeg \
    busybox e2fsprogs \
    systemd systemd-sysv openssh-server sudo \
    chrony socat fuse3 iptables nfs-common \
    && rm -rf /var/lib/apt/lists/*

# ── Node.js 22 LTS via NodeSource ────────────────────────────────────────────
RUN curl -fsSL https://deb.nodesource.com/setup_22.x | bash - \
    && apt-get install -y --no-install-recommends nodejs \
    && npm install -g yarn typescript ts-node \
    && node --version && npm --version \
    && rm -rf /var/lib/apt/lists/*

# ── Symlink pip3/pip into /usr/bin ────────────────────────────────────────────
RUN ln -sf /usr/local/bin/pip3 /usr/bin/pip3 && \
    ln -sf /usr/local/bin/pip  /usr/bin/pip

# ── System-wide PATH ──────────────────────────────────────────────────────────
RUN printf 'PATH="/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"\n' \
        > /etc/environment && \
    printf 'export PATH="/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"\n' \
        > /etc/profile.d/pandastack-path.sh && \
    chmod +x /etc/profile.d/pandastack-path.sh

# ── Full data science + AI stack (pinned versions) ────────────────────────────
RUN pip3 install --no-cache-dir \
    # Jupyter
    jupyter-server==2.16.0 ipykernel==6.29.5 ipython==9.2.0 notebook jupyterlab \
    # Core data science
    numpy==2.3.5 pandas==2.2.3 scipy==1.17.1 scikit-learn==1.6.1 scikit-image==0.25.2 \
    xarray==2025.4.0 statsmodels sympy==1.14.0 numba==0.63.1 \
    # Visualisation
    matplotlib==3.10.8 seaborn==0.13.2 plotly==6.0.1 bokeh==3.9.0 kaleido==1.0.0 \
    # Image / media
    pillow==12.1.1 imageio==2.37.3 opencv-python==4.11.0.86 \
    # Audio / signal processing
    librosa==0.11.0 soundfile==0.13.1 \
    # NLP
    nltk==3.9.3 spacy==3.8.11 textblob==0.19.0 gensim==4.4.0 \
    # Office / document formats
    openpyxl==3.1.5 xlrd==2.0.2 python-docx==1.1.2 \
    # Networking / serialisation
    aiohttp==3.13.3 requests==2.32.5 httpx==0.28.1 urllib3==2.6.3 \
    beautifulsoup4==4.14.3 orjson==3.11.7 \
    # Finance / misc
    yfinance joblib==1.5.3 \
    # DB / serialisation
    psycopg2-binary dill==0.3.9 sqlalchemy==2.0.49 alembic \
    # Web server / AI
    fastapi "uvicorn[standard]" pydantic==2.11.10 python-dotenv==1.1.1 \
    openai==2.31.0 anthropic==0.94.0 \
    # Agent frameworks (folded in from the former 'openai-agents' template)
    openai-agents tiktoken \
    # Data serialisation
    pyarrow==23.0.1 \
    # Dev / testing
    pytest==8.3.5 \
    # Browser automation
    playwright \
    && python3 -m ipykernel install --sys-prefix \
    && python3 -m playwright install chromium --with-deps

# ── LSP-as-a-Service: baked in so there is zero per-sandbox install cost ──────
# pylsp-rope enables workspace/symbol; without it pylsp returns -32601.
RUN pip3 install --no-cache-dir \
    'python-lsp-server>=1.14' pylsp-rope pyflakes pycodestyle

# ── Persist PLAYWRIGHT_BROWSERS_PATH across all Python processes ──────────────
# Docker ENV vars are NOT injected into Firecracker microVMs; sitecustomize.py
# runs before any Python script regardless of invocation method.
RUN SITEDIR=$(python3 -c "import site; print(site.getsitepackages()[0])") && \
    printf 'import os\nif not os.environ.get("PLAYWRIGHT_BROWSERS_PATH"):\n    os.environ["PLAYWRIGHT_BROWSERS_PATH"] = "/opt/playwright"\n' \
    > "$SITEDIR/sitecustomize.py"

WORKDIR /workspace
ENV PYTHONUNBUFFERED=1
CMD ["/bin/bash"]
