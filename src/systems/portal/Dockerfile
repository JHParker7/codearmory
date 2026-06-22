FROM node:22-alpine AS build-spa
WORKDIR /spa
COPY package*.json ./
RUN npm ci
COPY src ./src
COPY index.html tsconfig.json vite.config.ts ./
RUN npm run build

FROM node:22-alpine AS build-bff
WORKDIR /bff
COPY bff/package*.json ./
RUN npm ci
COPY bff/src ./src
COPY bff/tsconfig.json ./
RUN npm run build

FROM node:22-alpine
WORKDIR /app
COPY bff/package*.json ./
RUN npm ci --omit=dev
COPY --from=build-bff /bff/dist ./dist
COPY --from=build-spa /spa/dist ./public
ENV NODE_ENV=production
EXPOSE 3001
CMD ["node", "dist/index.js"]
