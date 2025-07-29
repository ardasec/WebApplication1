using Microsoft.EntityFrameworkCore;
using WebApplication1.Data;
using WebApplication1.Models;

var builder = WebApplication.CreateBuilder(args);

// Configure EF Core with SQLite
builder.Services.AddDbContext<AppDbContext>(options =>
    options.UseSqlite("Data Source=app.db"));

// Add Swagger/OpenAPI
builder.Services.AddEndpointsApiExplorer();
builder.Services.AddSwaggerGen();
builder.Services.AddRazorPages();

var app = builder.Build();

// Ensure SQLite database file and schema exist
using (var scope = app.Services.CreateScope())
{
    var db = scope.ServiceProvider.GetRequiredService<AppDbContext>();
    db.Database.EnsureCreated();
}

if (app.Environment.IsDevelopment())
{
    app.UseSwagger();
    app.UseSwaggerUI();
}
app.UseStaticFiles();

app.MapRazorPages();

// Map endpoints
app.MapPost("/register", async (RegisterDto dto, AppDbContext db) =>
{
    if (string.IsNullOrWhiteSpace(dto.Username) || string.IsNullOrWhiteSpace(dto.Password))
        return Results.BadRequest("Username and password are required");

    var exists = await db.Users.AnyAsync(u => u.Username == dto.Username);
    if (exists)
        return Results.BadRequest("Username already taken");

    var user = new User
    {
        Username = dto.Username,
        PasswordHash = dto.Password // NOTE: For demo only – store hashed passwords in production
    };
    db.Users.Add(user);
    await db.SaveChangesAsync();

    return Results.Ok(new { user.Id, user.Username });
}).WithName("RegisterUser");

app.MapPost("/login", async (LoginDto dto, AppDbContext db) =>
{
    var user = await db.Users.FirstOrDefaultAsync(u => u.Username == dto.Username &&
                                                      u.PasswordHash == dto.Password);
    return user is null
        ? Results.Unauthorized()
        : Results.Ok(new { user.Id, user.Username });
}).WithName("LoginUser");

app.MapGet("/products", async (AppDbContext db) => await db.Products.ToListAsync())
   .WithName("GetProducts");

app.MapPost("/products", async (Product product, AppDbContext db) =>
{
    db.Products.Add(product);
    await db.SaveChangesAsync();
    return Results.Created($"/products/{product.Id}", product);
}).WithName("CreateProduct");

app.MapPost("/orders", async (CreateOrderDto dto, AppDbContext db) =>
{
    var userExists = await db.Users.AnyAsync(u => u.Id == dto.UserId);
    if (!userExists) return Results.BadRequest("User not found");

    var order = new Order { UserId = dto.UserId };

    foreach (var item in dto.Items)
    {
        var product = await db.Products.FindAsync(item.ProductId);
        if (product is null) return Results.BadRequest($"Product {item.ProductId} not found");

        order.Items.Add(new OrderItem
        {
            ProductId = item.ProductId,
            Quantity = item.Quantity
        });
    }

    db.Orders.Add(order);
    await db.SaveChangesAsync();

    return Results.Ok(order);
}).WithName("CreateOrder");

app.Run();

// DTOs
record RegisterDto(string Username, string Password);
record LoginDto(string Username, string Password);
record CreateOrderItemDto(int ProductId, int Quantity);
record CreateOrderDto(int UserId, List<CreateOrderItemDto> Items);