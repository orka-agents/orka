# The data-analysis job for demo 14. Copies only; no build steps run in the
# target platform, so it builds for linux/amd64 from any host.
FROM python:3.12-slim
ENV PYTHONDONTWRITEBYTECODE=1 PYTHONUNBUFFERED=1
COPY analyze.py sensor-history.csv /analysis/
USER 65532:65532
ENTRYPOINT ["python3", "/analysis/analyze.py"]
