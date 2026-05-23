FROM public.ecr.aws/docker/library/python:3.12-alpine
WORKDIR /tests
COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt
COPY . .
CMD ["pytest", "-v"]
