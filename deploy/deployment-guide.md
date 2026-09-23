# DASYIN ERP Backend Deployment Guide

This guide provides multiple deployment options for the DASYIN ERP Backend to cloud platforms in regions close to Adelaide, Australia.

## Quick Start Database Setup

If you just want to run the SQL script to set up the database, use the `database-schema.sql` file:

```bash
# Connect to PostgreSQL
psql -U postgres -h your-host -p 5432

# Create database
CREATE DATABASE das_erp_production;

# Connect to the database
\c das_erp_production;

# Run the schema script
\i deploy/database-schema.sql
```

## Deployment Options

### 1. AWS ECS (Recommended for Production)

**Region:** `ap-southeast-2` (Sydney) - Closest to Adelaide

#### Prerequisites
- AWS CLI installed and configured
- Docker installed
- Terraform installed (optional, for infrastructure as code)

#### Manual Deployment Steps

1. **Create ECR Repository**
```bash
aws ecr create-repository --repository-name das-erp-backend --region ap-southeast-2
```

2. **Build and Push Docker Image**
```bash
# Get login token
aws ecr get-login-password --region ap-southeast-2 | docker login --username AWS --password-stdin YOUR_ACCOUNT.dkr.ecr.ap-southeast-2.amazonaws.com

# Build image
docker build -t das-erp-backend .

# Tag image
docker tag das-erp-backend:latest YOUR_ACCOUNT.dkr.ecr.ap-southeast-2.amazonaws.com/das-erp-backend:latest

# Push image
docker push YOUR_ACCOUNT.dkr.ecr.ap-southeast-2.amazonaws.com/das-erp-backend:latest
```

3. **Create RDS Instance**
```bash
aws rds create-db-instance \
    --db-instance-identifier das-erp-db \
    --db-instance-class db.t3.micro \
    --engine postgres \
    --master-username postgres \
    --master-user-password YOUR_SECURE_PASSWORD \
    --allocated-storage 20 \
    --db-name das_erp_production \
    --region ap-southeast-2
```

4. **Create ECS Cluster and Service**
Use the provided `aws-ecs-task-definition.json` and deploy via AWS Console or CLI.

#### Terraform Deployment (Automated)

1. **Navigate to Terraform directory**
```bash
cd deploy/terraform
```

2. **Copy and customize variables**
```bash
cp terraform.tfvars.example terraform.tfvars
# Edit terraform.tfvars with your values
```

3. **Deploy infrastructure**
```bash
terraform init
terraform plan
terraform apply
```

### 2. Fly.io (Simple and Fast)

**Region:** `syd` (Sydney)

#### Prerequisites
- Fly CLI installed
- Docker installed

#### Deployment Steps

1. **Login to Fly**
```bash
fly auth login
```

2. **Initialize app**
```bash
fly launch --copy-config --name das-erp-backend
```

3. **Create PostgreSQL database**
```bash
fly postgres create --name das-erp-postgres --region syd
```

4. **Connect database to app**
```bash
fly postgres attach --app das-erp-backend das-erp-postgres
```

5. **Set secrets**
```bash
fly secrets set JWT_SECRET="your-super-secure-jwt-secret-key"
fly secrets set API_KEY="your-api-key"
```

6. **Deploy**
```bash
fly deploy
```

### 3. Render (Beginner-Friendly)

**Region:** Singapore (closest available to Adelaide)

#### Deployment Steps

1. **Connect GitHub Repository**
   - Sign up at render.com
   - Connect your GitHub repository

2. **Create PostgreSQL Database**
   - Create new PostgreSQL service
   - Note connection details

3. **Create Web Service**
   - Use the provided `render.yaml` configuration
   - Set environment variables in Render dashboard:
     - `JWT_SECRET`
     - `API_KEY`

4. **Deploy**
   - Render will automatically deploy on git push

### 4. Railway (Developer-Friendly)

**Region:** Asia Pacific (Singapore)

#### Deployment Steps

1. **Install Railway CLI**
```bash
npm install -g @railway/cli
```

2. **Login and initialize**
```bash
railway login
railway init
```

3. **Add PostgreSQL**
```bash
railway add postgresql
```

4. **Deploy**
```bash
railway up
```

### 5. Google Cloud Run

**Region:** `australia-southeast1` (Sydney)

#### Prerequisites
- Google Cloud CLI installed
- Docker installed

#### Deployment Steps

1. **Enable APIs and set project**
```bash
gcloud config set project YOUR_PROJECT_ID
gcloud services enable run.googleapis.com
gcloud services enable sql-component.googleapis.com
```

2. **Create Cloud SQL instance**
```bash
gcloud sql instances create das-erp-db \
    --database-version=POSTGRES_15 \
    --tier=db-f1-micro \
    --region=australia-southeast1
```

3. **Build and deploy**
```bash
# Build container
gcloud builds submit --tag gcr.io/YOUR_PROJECT_ID/das-erp-backend

# Deploy to Cloud Run
gcloud run deploy das-erp-backend \
    --image gcr.io/YOUR_PROJECT_ID/das-erp-backend \
    --platform managed \
    --region australia-southeast1 \
    --allow-unauthenticated
```

## Environment Variables Required

All deployment methods require these environment variables:

### Required
- `DATABASE_URL` or individual DB variables (`DB_HOST`, `DB_PORT`, `DB_NAME`, `DB_USER`, `DB_PASSWORD`)
- `JWT_SECRET` (minimum 32 characters)
- `PORT` (usually 8080)
- `ENVIRONMENT` (production)

### Optional
- `API_KEY`
- `CORS_ORIGINS`
- `LOG_LEVEL`
- `DEFAULT_ORG_ID`

## Database Initialization

After deployment, the database schema will be automatically created via GORM migrations. However, if you prefer to run the SQL script manually:

1. Connect to your production database
2. Run the `deploy/database-schema.sql` script
3. This will create all tables, indexes, triggers, and default data

## Health Checks

All deployments include health check endpoints:
- `/health` - Basic health check
- `/api/v1/health` - Detailed API health check

## Security Considerations

1. **Always use HTTPS in production**
2. **Set strong passwords and secrets**
3. **Restrict database access to application servers only**
4. **Enable database backups**
5. **Monitor logs and set up alerting**

## Cost Estimates (Monthly)

### AWS (Sydney region)
- **Minimal**: ~$50/month (t3.micro RDS + Fargate)
- **Production**: ~$200/month (t3.small RDS + multiple Fargate tasks)

### Fly.io
- **Minimal**: ~$25/month (shared CPU + 1GB Postgres)
- **Production**: ~$80/month (dedicated CPU + larger Postgres)

### Render
- **Minimal**: ~$25/month (starter plans)
- **Production**: ~$75/month (standard plans)

### Railway
- **Minimal**: ~$20/month (usage-based pricing)

### Google Cloud Run
- **Minimal**: ~$15/month (pay-per-request)
- **Production**: ~$50/month (with Cloud SQL)

## Support

For deployment issues, check:
1. Application logs
2. Database connectivity
3. Environment variables
4. Security group/firewall rules
5. Health check endpoints

## Recommended Production Setup

For production use near Adelaide:

1. **Primary**: AWS ECS in `ap-southeast-2` (Sydney)
   - Most comprehensive services
   - Best performance for Adelaide
   - Full control and scalability

2. **Alternative**: Fly.io in `syd` region
   - Simpler setup
   - Good performance
   - Cost-effective
   - Built-in global CDN