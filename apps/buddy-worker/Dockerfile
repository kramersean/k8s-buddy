FROM python:3.11-slim AS builder

WORKDIR /app

COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt

COPY main.py .

FROM python:3.11-slim

WORKDIR /app

COPY --from=builder /usr/local/lib/python3.11/site-packages /usr/local/lib/python3.11/site-packages
COPY --from=builder /app/main.py .

ENV FLASK_APP=main.py
ENV FLASK_ENV=production
ENV OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4317
ENV PORT=8081

EXPOSE 8081

CMD ["python", "main.py"]
