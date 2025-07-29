using Microsoft.AspNetCore.Mvc.RazorPages;
using Microsoft.EntityFrameworkCore;
using WebApplication1.Data;
using WebApplication1.Models;

namespace WebApplication1.Pages;

public partial class IndexModel : PageModel
{
    private readonly AppDbContext _db;

    public List<Product> Products { get; private set; } = new();

    public IndexModel(AppDbContext db)
    {
        _db = db;
    }

    public async Task OnGetAsync()
    {
        Products = await _db.Products.AsNoTracking().ToListAsync();
    }
} 