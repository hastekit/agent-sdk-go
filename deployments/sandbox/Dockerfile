# syntax=docker/dockerfile:1
FROM python:3.11-slim-bookworm
ENV MPLBACKEND=Agg
RUN apt-get update && apt-get install -y --no-install-recommends \
    bash ca-certificates curl wget git jq tini \
    && rm -rf /var/lib/apt/lists/*
COPY deployments/sandbox/requirements.txt /tmp/sandbox-requirements.txt
RUN pip install --no-cache-dir --only-binary=:all: -r /tmp/sandbox-requirements.txt \
    && pip check \
    && python -c "import numpy, pandas, matplotlib.pyplot, seaborn, PIL, requests, httpx, bs4, lxml.etree, yaml, openpyxl, xlsxwriter, pypdf, docx, pptx, reportlab" \
    && rm /tmp/sandbox-requirements.txt
ENV PYTHONUNBUFFERED=1 SANDBOX_ROOT=/workspace
WORKDIR /workspace
ENTRYPOINT ["/usr/bin/tini", "--"]
CMD ["sleep", "infinity"]
